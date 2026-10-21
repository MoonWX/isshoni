#include "im/log/ring.h"

#include <charconv>
#include <chrono>
#include <cstring>

#include "im/abi/status.h"
#include "im/abi/utf8.h"
#include "im/json/writer.h"

namespace im::log {

// A record keeps its strings in fixed arrays: pushing copies at most a few hundred bytes and never allocates.
template <std::size_t N> struct FixedString {
  char data[N];
  uint8_t len;

  void Set(std::string_view s) noexcept {
    static_assert(N <= 255);
    len = static_cast<uint8_t>(abi::Utf8PrefixLen(s, N));
    std::memcpy(data, s.data(), len);
  }
  std::string_view view() const noexcept { return std::string_view(data, len); }
};

struct Ring::Record {
  struct StoredAttr {
    FixedString<kMaxKeyBytes> key;
    Attr::Kind kind;
    union {
      int64_t i;
      uint64_t u;
      double d;
      bool b;
    };
    FixedString<kMaxStringBytes> str;
  };

  int64_t unix_ns;
  uint64_t dropped;
  uint32_t level;
  uint32_t attr_count;
  FixedString<kMaxComponentBytes> component;
  FixedString<kMaxMsgBytes> msg;
  StoredAttr attrs[kMaxAttrs];
};

struct Ring::Cell {
  std::atomic<uint64_t> seq;
  Record rec;
};

namespace {

constexpr std::string_view kLevelNames[] = {"error", "warn", "info", "debug"};

// The longest line Format can produce: every byte of every string may escape to 6 bytes ("\u001f").
constexpr std::size_t kEscape = 6;
constexpr std::size_t kWorstAttr = 1 + kEscape * kMaxKeyBytes + 2 + (2 + kEscape * kMaxStringBytes) + 1;
constexpr std::size_t kWorstLine = 6 + 24 + 11 + 5 + 15 + kEscape * kMaxComponentBytes + 9 + kEscape * kMaxMsgBytes +
                                   12 + 20 + 10 + kMaxAttrs * kWorstAttr + 2 + 1;
static_assert(kWorstLine <= static_cast<std::size_t>(kMaxLineBytes), "kMaxLineBytes is too small");

void Append2(std::string &out, unsigned v) {
  out.push_back(static_cast<char>('0' + v / 10));
  out.push_back(static_cast<char>('0' + v % 10));
}

// Howard Hinnant's civil_from_days: days since 1970-01-01 to a proleptic Gregorian date.
void CivilFromDays(int64_t z, int64_t &y, unsigned &m, unsigned &d) {
  z += 719468;
  const int64_t era = (z >= 0 ? z : z - 146096) / 146097;
  const auto doe = static_cast<unsigned>(z - era * 146097);
  const unsigned yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
  y = static_cast<int64_t>(yoe) + era * 400;
  const unsigned doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
  const unsigned mp = (5 * doy + 2) / 153;
  d = doy - (153 * mp + 2) / 5 + 1;
  m = mp < 10 ? mp + 3 : mp - 9;
  y += (m <= 2) ? 1 : 0;
}

void AppendTimestamp(std::string &out, int64_t unix_ns) {
  if (unix_ns < 0) {
    unix_ns = 0;
  }
  const int64_t ms = unix_ns / 1000000;
  const int64_t secs = ms / 1000;
  const int64_t days = secs / 86400;
  const auto sod = static_cast<unsigned>(secs % 86400);
  int64_t y = 0;
  unsigned m = 0;
  unsigned d = 0;
  CivilFromDays(days, y, m, d);
  char buf[24];
  const auto r = std::to_chars(buf, buf + sizeof buf, y);
  out.append(buf, r.ptr);
  out.push_back('-');
  Append2(out, m);
  out.push_back('-');
  Append2(out, d);
  out.push_back('T');
  Append2(out, sod / 3600);
  out.push_back(':');
  Append2(out, sod / 60 % 60);
  out.push_back(':');
  Append2(out, sod % 60);
  out.push_back('.');
  const auto milli = static_cast<unsigned>(ms % 1000);
  out.push_back(static_cast<char>('0' + milli / 100));
  Append2(out, milli % 100);
  out.push_back('Z');
}

void AppendString(std::string &out, std::string_view s) {
  out.push_back('"');
  json::AppendEscaped(out, s);
  out.push_back('"');
}

} // namespace

Ring::Ring(uint32_t capacity) : mask_(capacity - 1) {
  if (capacity < 2 || (capacity & (capacity - 1)) != 0) {
    mask_ = kCapacity - 1; // not a power of two: use the default size
  }
  cells_ = std::make_unique<Cell[]>(mask_ + 1);
  for (uint32_t i = 0; i <= mask_; ++i) {
    cells_[i].seq.store(i, std::memory_order_relaxed);
  }
  line_.reserve(static_cast<std::size_t>(kMaxLineBytes));
}

Ring::~Ring() = default;

void Ring::SetLevel(uint32_t level) noexcept {
  level_.store(level > IM_LOG_DEBUG ? IM_LOG_DEBUG : level, std::memory_order_relaxed);
}

bool Ring::Push(uint32_t level, std::string_view component, std::string_view msg,
                std::initializer_list<Attr> attrs) noexcept {
  if (!Enabled(level > IM_LOG_DEBUG ? IM_LOG_DEBUG : level)) {
    return false; // checked before reading the clock
  }
  const auto now = std::chrono::system_clock::now().time_since_epoch();
  return PushAt(std::chrono::duration_cast<std::chrono::nanoseconds>(now).count(), level, component, msg, attrs);
}

bool Ring::PushAt(int64_t unix_ns, uint32_t level, std::string_view component, std::string_view msg,
                  std::initializer_list<Attr> attrs) noexcept {
  if (level > IM_LOG_DEBUG) {
    level = IM_LOG_DEBUG;
  }
  if (!Enabled(level)) {
    return false;
  }
  // Vyukov's bounded queue: a cell is free for position pos when its seq equals pos, and readable when it is pos + 1.
  uint64_t pos = enqueue_pos_.load(std::memory_order_relaxed);
  Cell *cell = nullptr;
  for (;;) {
    cell = &cells_[pos & mask_];
    const uint64_t seq = cell->seq.load(std::memory_order_acquire);
    const auto diff = static_cast<int64_t>(seq - pos);
    if (diff == 0) {
      if (enqueue_pos_.compare_exchange_weak(pos, pos + 1, std::memory_order_relaxed)) {
        break;
      }
    } else if (diff < 0) {
      dropped_.fetch_add(1, std::memory_order_relaxed); // full: the consumer hasn't freed this cell yet
      return false;
    } else {
      pos = enqueue_pos_.load(std::memory_order_relaxed);
    }
  }

  Record &r = cell->rec;
  r.unix_ns = unix_ns;
  r.level = level;
  r.component.Set(component);
  r.msg.Set(msg);
  r.attr_count = 0;
  for (const Attr &a : attrs) {
    if (r.attr_count == kMaxAttrs) {
      break;
    }
    Record::StoredAttr &s = r.attrs[r.attr_count++];
    s.key.Set(a.key());
    s.kind = a.kind();
    switch (a.kind()) {
    case Attr::Kind::kInt:
      s.i = a.int_value();
      break;
    case Attr::Kind::kUint:
      s.u = a.uint_value();
      break;
    case Attr::Kind::kDouble:
      s.d = a.double_value();
      break;
    case Attr::Kind::kBool:
      s.b = a.bool_value();
      break;
    case Attr::Kind::kString:
      s.str.Set(a.string_value());
      break;
    }
  }
  r.dropped = dropped_.exchange(0, std::memory_order_relaxed);
  cell->seq.store(pos + 1, std::memory_order_release);
  return true;
}

int32_t Ring::Drain(char *buf, int32_t cap) {
  if (buf == nullptr || cap <= kMaxLineBytes) {
    return IM_ERR_INVALID_ARG;
  }
  std::lock_guard<std::mutex> lock(drain_mu_);
  const auto room = static_cast<std::size_t>(cap) - 1;
  std::size_t used = 0;
  for (;;) {
    Cell &cell = cells_[dequeue_pos_ & mask_];
    if (cell.seq.load(std::memory_order_acquire) != dequeue_pos_ + 1) {
      break; // empty, or the next record is still being written
    }
    const Record &r = cell.rec;
    line_.clear();
    line_ += "{\"t\":\"";
    AppendTimestamp(line_, r.unix_ns);
    line_ += "\",\"level\":\"";
    line_ += kLevelNames[r.level];
    line_ += "\",\"component\":";
    AppendString(line_, r.component.view());
    line_ += ",\"msg\":";
    AppendString(line_, r.msg.view());
    if (r.dropped > 0) {
      line_ += ",\"dropped\":";
      char num[24];
      const auto res = std::to_chars(num, num + sizeof num, r.dropped);
      line_.append(num, res.ptr);
    }
    line_ += ",\"attrs\":{";
    for (uint32_t k = 0; k < r.attr_count; ++k) {
      const Record::StoredAttr &a = r.attrs[k];
      if (k > 0) {
        line_.push_back(',');
      }
      AppendString(line_, a.key.view());
      line_.push_back(':');
      switch (a.kind) {
      case Attr::Kind::kInt: {
        char num[24];
        const auto res = std::to_chars(num, num + sizeof num, a.i);
        line_.append(num, res.ptr);
        break;
      }
      case Attr::Kind::kUint: {
        char num[24];
        const auto res = std::to_chars(num, num + sizeof num, a.u);
        line_.append(num, res.ptr);
        break;
      }
      case Attr::Kind::kDouble:
        json::AppendDouble(line_, a.d);
        break;
      case Attr::Kind::kBool:
        line_ += a.b ? "true" : "false";
        break;
      case Attr::Kind::kString:
        AppendString(line_, a.str.view());
        break;
      }
    }
    line_ += "}}\n";
    if (used + line_.size() > room) {
      break; // stays queued for the next call
    }
    std::memcpy(buf + used, line_.data(), line_.size());
    used += line_.size();
    cell.seq.store(dequeue_pos_ + mask_ + 1, std::memory_order_release);
    ++dequeue_pos_;
  }
  buf[used] = '\0';
  return static_cast<int32_t>(used);
}

Ring &Default() noexcept {
  static Ring *ring = new Ring(kCapacity); // leaked on purpose
  return *ring;
}

std::string FormatTimestamp(int64_t unix_ns) {
  std::string s;
  AppendTimestamp(s, unix_ns);
  return s;
}

} // namespace im::log
