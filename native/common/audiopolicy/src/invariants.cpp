// The invariants I1-I5 of docs/m2/05-windows-audio.md §6.3, checked with algorithms of their own: descendant searches
// over the table's parent links instead of the planner's ancestor walks.
#include <deque>
#include <string>
#include <vector>

#include "im/audiopolicy/planner.h"

namespace im::audiopolicy {
namespace {

// The children of every process along strict or loose links.
std::vector<std::vector<std::size_t>> Children(const ProcessTable &t, bool loose) {
  std::vector<std::vector<std::size_t>> kids(t.size());
  for (std::size_t i = 0; i < t.size(); ++i) {
    const int32_t p = t.Parent(i, loose);
    if (p >= 0) {
      kids[static_cast<std::size_t>(p)].push_back(i);
    }
  }
  return kids;
}

// root and everything below it.
std::vector<char> Descendants(const std::vector<std::vector<std::size_t>> &kids, std::size_t root) {
  std::vector<char> in(kids.size(), 0);
  std::deque<std::size_t> q{root};
  in[root] = 1;
  while (!q.empty()) {
    const std::size_t i = q.front();
    q.pop_front();
    for (const std::size_t c : kids[i]) {
      if (!in[c]) {
        in[c] = 1;
        q.push_back(c);
      }
    }
  }
  return in;
}

std::string KeyText(const ProcessKey &k) { return std::to_string(k.pid) + "/" + std::to_string(k.created); }

} // namespace

std::vector<Violation> CheckInvariants(const ProcessTable &t, const PlanInput &in, const Plan &plan,
                                       const Exclusions &ex, const PlannerOptions &opts) {
  std::vector<Violation> out;
  const auto kids_strict = Children(t, false);
  const auto kids_loose = Children(t, true);
  const auto &kids_i1 = opts.loose_links ? kids_loose : kids_strict;

  // isshoni's own tree: the SELF instance rules' processes and everything below them (loose, as row 1 matches).
  std::vector<char> self_tree(t.size(), 0);
  for (const Rule &r : ex.rules) {
    if (r.kind == IM_RULE_INSTANCE && r.source == IM_RULE_SRC_SELF) {
      if (const auto k = t.Find(r.pid)) {
        const auto d = Descendants(kids_loose, *k);
        for (std::size_t i = 0; i < t.size(); ++i) {
          self_tree[i] = static_cast<char>(self_tree[i] | d[i]);
        }
      }
    }
  }
  const auto kept_out = [&](std::size_t i) { return plan.processes.at(i).verdict == Verdict::kKeptOut; };

  // The streams that run after the plan: live ones it doesn't remove, and the ones it opens.
  std::vector<char> removed(in.streams.size(), 0);
  for (const Removal &r : plan.remove) {
    if (r.stream < removed.size()) {
      removed[r.stream] = 1;
    }
  }
  std::vector<LiveStream> running;
  for (std::size_t j = 0; j < in.streams.size(); ++j) {
    if (!removed[j]) {
      running.push_back(in.streams[j]);
    }
  }
  for (const Activation &a : plan.activate) {
    LiveStream s;
    s.kind = a.kind;
    s.target = a.target;
    s.parent_pid = a.parent_pid;
    s.exe = a.exe;
    running.push_back(std::move(s));
  }

  std::size_t exclusive = 0;
  for (const LiveStream &s : running) {
    if (s.kind != StreamKind::kInclude) {
      ++exclusive;
      continue;
    }
    // I3: the planned key.
    const auto i = t.Find(s.target.pid);
    const bool same = i && (s.target.created != 0 ? t.at(*i).key.created == s.target.created
                                                  : (t.at(*i).key.created == 0 && t.at(*i).exe == s.exe &&
                                                     t.at(*i).parent_pid == s.parent_pid));
    if (!same) {
      out.push_back(Violation{Invariant::kI3, s.target, {}, "stream " + KeyText(s.target) + " lost its process"});
      continue;
    }
    const ProcessKey key = t.at(*i).key;
    // I2: never a launcher, a kept-out process or isshoni's tree.
    if (kept_out(*i) || plan.processes.at(*i).launcher || self_tree[*i]) {
      out.push_back(Violation{Invariant::kI2, key, key, "include stream on a forbidden target " + KeyText(key)});
    }
    // I1: no kept-out process in the target's OS tree.
    const auto d = Descendants(kids_i1, *i);
    for (std::size_t x = 0; x < t.size(); ++x) {
      if (d[x] && x != *i && kept_out(x)) {
        out.push_back(Violation{Invariant::kI1, key, t.at(x).key,
                                "kept-out " + KeyText(t.at(x).key) + " in the tree of " + KeyText(key)});
      }
    }
  }
  // I5: one exclusive stream (the one it replaces fades out in the crossfade).
  if (exclusive > 1) {
    out.push_back(Violation{Invariant::kI5, {}, {}, std::to_string(exclusive) + " exclusive streams"});
  }
  // I4: the fast path's tree holds every kept-out process and no heard session process.
  if (plan.engine == Engine::kExcludeSelf) {
    const auto r = t.Find(plan.exclude_target);
    if (!r) {
      out.push_back(Violation{Invariant::kI4, plan.exclude_target, {}, "R is not in the table"});
    } else {
      const auto strict = Descendants(kids_strict, *r);
      const auto loose = Descendants(kids_loose, *r);
      for (std::size_t x = 0; x < t.size(); ++x) {
        if (kept_out(x) && !strict[x]) {
          out.push_back(Violation{Invariant::kI4, plan.exclude_target, t.at(x).key,
                                  "kept-out " + KeyText(t.at(x).key) + " outside R's tree"});
        }
      }
      for (const Session &s : in.sessions) {
        const auto i = t.Find(s.pid);
        if (s.flow == Flow::kRender && i && !kept_out(*i) && loose[*i]) {
          out.push_back(Violation{Invariant::kI4, plan.exclude_target, t.at(*i).key,
                                  "heard session " + KeyText(t.at(*i).key) + " inside R's tree"});
        }
      }
    }
  }
  return out;
}

std::vector<Violation> CheckExclusiveStreams(std::span<const LiveStream> streams, bool crossfading) {
  std::size_t exclusive = 0;
  for (const LiveStream &s : streams) {
    if (s.kind != StreamKind::kInclude) {
      ++exclusive;
    }
  }
  std::vector<Violation> out;
  if (exclusive > (crossfading ? 2u : 1u)) {
    out.push_back(Violation{Invariant::kI5, {}, {}, std::to_string(exclusive) + " exclusive streams"});
  }
  return out;
}

} // namespace im::audiopolicy
