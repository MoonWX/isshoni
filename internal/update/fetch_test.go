package update

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The redirect rule (06 §8.1): https only, to github.com and *.githubusercontent.com, at most 5.
func TestAllowRedirect(t *testing.T) {
	ep := defaultEndpoints()
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://github.com/MoonWX/isshoni/releases/download/v0.2.1/x", true},
		{"https://GitHub.com/x", true},
		{"https://github.com:443/x", true},
		{"https://objects.githubusercontent.com/github-production-release-asset/1?X-Amz-Signature=x", true},
		{"https://release-assets.githubusercontent.com/x", true},
		{"http://github.com/x", false},
		{"http://objects.githubusercontent.com/x", false},
		{"https://github.com:8443/x", false},
		{"https://githubusercontent.com/x", false},
		{"https://evilgithubusercontent.com/x", false},
		{"https://github.com.evil.example/x", false},
		{"https://api.github.com/x", false},
		{"https://www.github.com/x", false},
		{"https://user:pass@github.com/x", false},
		{"https://127.0.0.1/x", false},
	} {
		u, err := url.Parse(tt.url)
		if err != nil {
			t.Fatal(err)
		}
		if err := ep.allowRedirect(u); (err == nil) != tt.ok {
			t.Errorf("allowRedirect(%s) = %v, want ok %t", tt.url, err, tt.ok)
		} else if err != nil && strings.Contains(err.Error(), "X-Amz") {
			t.Errorf("allowRedirect(%s) = %v names the query", tt.url, err)
		}
	}
}

func TestRedirectsRefused(t *testing.T) {
	for _, tt := range []struct {
		name, to string
	}{
		{"another host", "https://evil.example/isshoni-desktop-update.json"},
		{"http", "http://github.com" + repoPath + "/download/v0.2.1/" + ManifestName},
		{"http asset host", "http://" + assetHost + assetPrefix + "v0.2.1/" + ManifestName},
		{"another port", "https://github.com:8443" + repoPath + "/download/v0.2.1/" + ManifestName},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == repoPath+"/latest/download/"+ManifestName {
						redirect(w, tt.to)
						return true
					}
					return false
				})
				st, err := h.u.CheckNow(t.Context())
				if st.State != StateError || CodeOf(err) != CodeNetwork || !strings.Contains(err.Error(), "refused") {
					t.Fatalf("CheckNow = %+v, %v; want a refused redirect", st, err)
				}
				if reqs := h.gh.requests(); len(reqs) != 1 {
					t.Errorf("requests %v, want only the first", reqs)
				}
			})
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	for _, tt := range []struct {
		hops  int
		state State
	}{{5, StateReady}, {6, StateError}} {
		t.Run(strconv.Itoa(tt.hops), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				// The manifest's latest URL redirects to hop/1, …, which lead to the release's URL, which redirects to
				// the asset host: hops redirects in all.
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path == repoPath+"/latest/download/"+ManifestName {
						redirect(w, "https://github.com/hop/1")
						return true
					}
					if n, ok := strings.CutPrefix(r.URL.Path, "/hop/"); ok {
						i, _ := strconv.Atoi(n)
						if i < tt.hops-2 {
							redirect(w, "https://github.com/hop/"+strconv.Itoa(i+1))
						} else {
							redirect(w, "https://github.com"+repoPath+"/download/v0.2.1/"+ManifestName)
						}
						return true
					}
					return false
				})
				st, err := h.u.CheckNow(t.Context())
				if st.State != tt.state {
					t.Errorf("CheckNow with %d redirects = %+v, %v; want %s", tt.hops, st, err, tt.state)
				}
			})
		})
	}
}

// The manifest and the signature: 20 s each.
func TestFetchTimeouts(t *testing.T) {
	for _, asset := range []string{ManifestName, SignatureName} {
		t.Run(asset, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.Host == assetHost && strings.HasSuffix(r.URL.Path, "/"+asset) {
						select { // hang until the client gives up
						case <-r.Context().Done():
						case <-h.gh.quit:
						}
						return true
					}
					return false
				})
				start := time.Now()
				st, err := h.u.CheckNow(t.Context())
				if st.State != StateError || CodeOf(err) != CodeNetwork {
					t.Fatalf("CheckNow = %+v, %v; want error/network", st, err)
				}
				if d := time.Since(start); d != fetchTimeout {
					t.Errorf("gave up after %v, want %v", d, fetchTimeout)
				}
			})
		})
	}
}

// The installer: a stall of 60 s without a byte, and 60 min in all; the partial file is deleted.
func TestDownloadTimeouts(t *testing.T) {
	for _, tt := range []struct {
		name  string
		every time.Duration // a byte every …; 0: half the body, then nothing
		after time.Duration // the download's age when it fails
		cause string
	}{
		{"stall", 0, stallTimeout, errStalled.Error()},
		{"total", 59 * time.Second, downloadTimeout, errTotalTimeout.Error()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(1000, 2))
				var started time.Time
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.Host != assetHost || !strings.HasSuffix(r.URL.Path, rel.name) {
						return false
					}
					started = time.Now()
					w.Header().Set("Content-Length", strconv.Itoa(len(rel.installer)))
					rc := http.NewResponseController(w)
					if tt.every == 0 {
						_, _ = w.Write(rel.installer[:500])
						_ = rc.Flush()
						select {
						case <-r.Context().Done():
						case <-h.gh.quit:
						}
						return true
					}
					for _, b := range rel.installer {
						_, _ = w.Write([]byte{b})
						_ = rc.Flush()
						select {
						case <-time.After(tt.every):
						case <-r.Context().Done():
							return true
						}
					}
					return true
				})
				st, err := h.u.CheckNow(t.Context())
				if st.State != StateError || CodeOf(err) != CodeDownloadFailed || !strings.Contains(err.Error(), tt.cause) {
					t.Fatalf("CheckNow = %+v, %v; want download_failed (%s)", st, err, tt.cause)
				}
				if d := time.Since(started); d != tt.after {
					t.Errorf("failed %v after the download started, want %v", d, tt.after)
				}
				h.wantFiles(stateName)
				if st.Available != "0.2.1" || h.u.Ready() != nil {
					t.Errorf("status %+v", st)
				}
			})
		})
	}
}

// The download publishes its progress.
func TestDownloadProgress(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(200<<10, 2))
		var mid Status
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Host != assetHost || !strings.HasSuffix(r.URL.Path, rel.name) {
				return false
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(rel.installer)))
			half := len(rel.installer) / 2
			_, _ = w.Write(rel.installer[:half])
			_ = http.NewResponseController(w).Flush()
			time.Sleep(time.Second) // the client reads the half meanwhile
			mid = h.u.Status()
			_, _ = w.Write(rel.installer[half:])
			return true
		})
		h.checkNow(StateReady, "")
		if mid.State != StateDownloading || mid.Progress < 0.49 || mid.Progress > 0.51 || mid.Available != "0.2.1" {
			t.Errorf("status halfway %+v, want downloading at 0.5", mid)
		}
	})
}

// Size and hash: a mismatch fails the download and deletes the partial file.
func TestDownloadMismatch(t *testing.T) {
	for _, tt := range []struct {
		name  string
		serve func(w http.ResponseWriter, good []byte)
		code  string
	}{
		{"other bytes", func(w http.ResponseWriter, good []byte) {
			bad := bytes.Clone(good)
			bad[len(bad)-1] ^= 1
			_, _ = w.Write(bad)
		}, CodeHashMismatch},
		{"longer, chunked", func(w http.ResponseWriter, good []byte) {
			_, _ = w.Write(good)
			_ = http.NewResponseController(w).Flush()
			_, _ = w.Write([]byte("more"))
		}, CodeSizeMismatch},
		{"shorter, chunked", func(w http.ResponseWriter, good []byte) {
			_, _ = w.Write(good[:10])
			_ = http.NewResponseController(w).Flush()
		}, CodeSizeMismatch},
		{"another Content-Length", func(w http.ResponseWriter, good []byte) {
			w.Header().Set("Content-Length", strconv.Itoa(len(good)-1))
			_, _ = w.Write(good[:len(good)-1])
		}, CodeSizeMismatch},
		{"HTTP 500", func(w http.ResponseWriter, good []byte) {
			w.WriteHeader(http.StatusInternalServerError)
		}, CodeDownloadFailed},
		{"brotli", func(w http.ResponseWriter, good []byte) {
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(good)
		}, CodeDownloadFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(70<<10, 2))
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.Host == assetHost && strings.HasSuffix(r.URL.Path, rel.name) {
						tt.serve(w, rel.installer)
						return true
					}
					return false
				})
				st, err := h.u.CheckNow(t.Context())
				if st.State != StateError || CodeOf(err) != tt.code || st.ErrorCode != tt.code {
					t.Fatalf("CheckNow = %+v, %v; want %s", st, err, tt.code)
				}
				h.wantFiles(stateName)
				if h.diskState().Pending != nil {
					t.Error("a pending update after a failed download")
				}
				if strings.Contains(err.Error(), "X-Amz") {
					t.Errorf("the error names the signed URL: %v", err)
				}
			})
		})
	}
}

// A gzip body (the Transport asks for it) is fine; another Content-Encoding is refused.
func TestContentEncoding(t *testing.T) {
	for _, tt := range []struct {
		encoding string
		state    State
	}{{"gzip", StateReady}, {"identity", StateReady}, {"br", StateError}, {"deflate", StateError}} {
		t.Run(tt.encoding, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.Host != assetHost || !strings.HasSuffix(r.URL.Path, "/"+ManifestName) {
						return false
					}
					w.Header().Set("Content-Encoding", tt.encoding)
					if tt.encoding == "gzip" {
						zw := gzip.NewWriter(w)
						_, _ = zw.Write(rel.manifest)
						_ = zw.Close()
						return true
					}
					_, _ = w.Write(rel.manifest)
					return true
				})
				st, err := h.u.CheckNow(t.Context())
				if st.State != tt.state || (tt.state == StateError && CodeOf(err) != CodeNetwork) {
					t.Errorf("CheckNow = %+v, %v; want %s", st, err, tt.state)
				}
			})
		})
	}
}

// The size limits of the manifest (64 KiB) and the signature (8 KiB), also after gzip.
func TestFetchSizeLimits(t *testing.T) {
	for _, tt := range []struct {
		name  string
		asset string
		size  int
		gzip  bool
		state State
	}{
		{"manifest at the limit", ManifestName, MaxManifestSize, false, StateReady},
		{"manifest above", ManifestName, MaxManifestSize + 1, false, StateError},
		{"manifest above, gzip", ManifestName, MaxManifestSize + 1, true, StateError},
		{"signature above", SignatureName, MaxSignatureSize + 1, false, StateError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				rel := h.gh.publish(h.active, "0.2.1", installerBytes(100, 2))
				body := pad(rel.manifest, max(tt.size, len(rel.manifest)))
				if tt.asset == ManifestName {
					// A padded manifest, correctly signed: only the fetch limit can refuse it.
					h.gh.with(func(fg *fakeGitHub) {
						fg.assets["v0.2.1/"+ManifestName] = body
						fg.assets["v0.2.1/"+SignatureName] = h.active.sign(body)
					})
				} else {
					body = append(bytes.Clone(rel.sig), bytes.Repeat([]byte("\n"), tt.size-len(rel.sig))...)
					h.gh.with(func(fg *fakeGitHub) { fg.assets["v0.2.1/"+SignatureName] = body })
				}
				if tt.gzip {
					h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
						if r.Host != assetHost || !strings.HasSuffix(r.URL.Path, "/"+tt.asset) {
							return false
						}
						w.Header().Set("Content-Encoding", "gzip")
						zw := gzip.NewWriter(w)
						_, _ = zw.Write(body)
						_ = zw.Close()
						return true
					})
				}
				st, err := h.u.CheckNow(t.Context())
				if st.State != tt.state || (tt.state == StateError && CodeOf(err) != CodeNetwork) {
					t.Errorf("CheckNow = %+v, %v; want %s", st, err, tt.state)
				}
			})
		})
	}
}

// The disk check: free space ≥ size + 64 MiB in the updates folder before downloading.
func TestDiskCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		rel := h.gh.publish(h.active, "0.2.1", installerBytes(1000, 2))
		h.free.Store(uint64(len(rel.installer)) + diskReserve - 1)
		h.checkNow(StateError, CodeDisk)
		if n := h.gh.count(rel.name); n != 0 {
			t.Errorf("%d installer requests without the space", n)
		}
		h.free.Store(uint64(len(rel.installer)) + diskReserve)
		time.Sleep(checkNowInterval)
		h.checkNow(StateReady, "")
	})
}

// A failure to write the downloaded file is disk.
func TestDownloadRenameFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.gh.publish(h.active, "0.2.1", installerBytes(1000, 2))
		h.mu.Lock()
		h.rename = func(oldpath, newpath string) error {
			if strings.HasSuffix(oldpath, ".partial") {
				return os.ErrPermission
			}
			return os.Rename(oldpath, newpath)
		}
		h.mu.Unlock()
		h.checkNow(StateError, CodeDisk)
		h.wantFiles(stateName)
	})
}

// The beta channel (06 §8.4): the API's listing, its headers, the newest eligible release with both assets.
func TestBetaChannel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		h.setChannel(ChannelBeta)
		h.gh.publish(h.active, "0.2.1", installerBytes(100, 1))
		h.gh.publish(h.active, "0.3.0-rc.1", installerBytes(100, 3))
		both := []string{ManifestName, SignatureName, "isshoni-desktop_0.3.0-rc.1_windows_amd64_setup.exe"}
		h.gh.setListing(
			listingEntry{tag: "deps-ffmpeg-9.0.2-r1", assets: []string{"ffmpeg.zip"}},
			listingEntry{tag: "v0.4.0", draft: true, assets: both},
			listingEntry{tag: "v0.0.1-ci.9", assets: both},
			listingEntry{tag: "v0.3.0-rc.1", assets: both},
			listingEntry{tag: "v0.2.1", assets: both},
		)
		st := h.checkNow(StateReady, "")
		if st.Available != "0.3.0-rc.1" {
			t.Errorf("status %+v, want 0.3.0-rc.1", st)
		}
		var api []seen
		for _, r := range h.gh.requests() {
			if r.host == "api.github.com" {
				api = append(api, r)
			}
		}
		if len(api) != 1 || api[0].path != "/repos/MoonWX/isshoni/releases" || api[0].query != "per_page=10" ||
			api[0].header.Get("Accept") != "application/vnd.github+json" ||
			api[0].header.Get("X-GitHub-Api-Version") != "2022-11-28" ||
			api[0].header.Get("User-Agent") != "isshoni-desktop/"+testCurrent {
			t.Errorf("API requests %+v", api)
		}
		if n := h.gh.count("/latest/"); n != 0 {
			t.Errorf("%d requests to releases/latest on the beta channel", n)
		}
	})
}

func TestBetaChannelErrors(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		body   []byte
		code   string
	}{
		{"403", http.StatusForbidden, nil, CodeRateLimited},
		{"429", http.StatusTooManyRequests, nil, CodeRateLimited},
		{"500", http.StatusInternalServerError, nil, CodeNetwork},
		{"not JSON", http.StatusOK, []byte("<html>"), CodeNetwork},
		{"empty", http.StatusOK, []byte("[]"), CodeNetwork},
		{"above 1 MiB", http.StatusOK, append(append([]byte("["), bytes.Repeat([]byte(" "), maxListingSize)...), ']'), CodeNetwork},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.setChannel(ChannelBeta)
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.Host != "api.github.com" {
						return false
					}
					w.WriteHeader(tt.status)
					_, _ = w.Write(tt.body)
					return true
				})
				h.checkNow(StateError, tt.code)
			})
		})
	}
}

func TestPickRelease(t *testing.T) {
	dl := func(tag, name string) string { return ReleasesURL + "/download/" + tag + "/" + name }
	both := func(tag string) string {
		return `{"name":"` + ManifestName + `","browser_download_url":"` + dl(tag, ManifestName) + `"},` +
			`{"name":"` + SignatureName + `","browser_download_url":"` + dl(tag, SignatureName) + `"}`
	}
	for _, tt := range []struct {
		name    string
		listing string
		test    bool
		want    string // the version picked; "" for none
	}{
		{"highest version, not listing order",
			`[{"tag_name":"v0.2.1","assets":[` + both("v0.2.1") + `]},{"tag_name":"v0.3.0-rc.2","assets":[` + both("v0.3.0-rc.2") + `]},{"tag_name":"v0.3.0-rc.1","assets":[` + both("v0.3.0-rc.1") + `]}]`,
			false, "0.3.0-rc.2"},
		{"final above its rc",
			`[{"tag_name":"v0.3.0-rc.1","assets":[` + both("v0.3.0-rc.1") + `]},{"tag_name":"v0.3.0","assets":[` + both("v0.3.0") + `]}]`,
			false, "0.3.0"},
		{"drafts skipped",
			`[{"tag_name":"v0.4.0","draft":true,"assets":[` + both("v0.4.0") + `]},{"tag_name":"v0.2.1","assets":[` + both("v0.2.1") + `]}]`,
			false, "0.2.1"},
		{"non-SemVer tags skipped",
			`[{"tag_name":"deps-ffmpeg-9.0.2-r1","assets":[]},{"tag_name":"0.9.0","assets":[` + both("0.9.0") + `]},{"tag_name":"v0.2.1","assets":[` + both("v0.2.1") + `]}]`,
			false, "0.2.1"},
		{"ci and dry-run versions skipped",
			`[{"tag_name":"v0.0.1-ci.9","assets":[` + both("v0.0.1-ci.9") + `]},{"tag_name":"v0.0.0-dryrun.1","assets":[` + both("v0.0.0-dryrun.1") + `]},{"tag_name":"v0.0.0","assets":[` + both("v0.0.0") + `]}]`,
			false, "0.0.0"},
		{"ci in an updatetest build",
			`[{"tag_name":"v0.0.1-ci.9","assets":[` + both("v0.0.1-ci.9") + `]},{"tag_name":"v0.0.0","assets":[` + both("v0.0.0") + `]}]`,
			true, "0.0.1-ci.9"},
		{"a missing signature",
			`[{"tag_name":"v0.3.0","assets":[{"name":"` + ManifestName + `","browser_download_url":"` + dl("v0.3.0", ManifestName) + `"}]},{"tag_name":"v0.2.1","assets":[` + both("v0.2.1") + `]}]`,
			false, "0.2.1"},
		{"another URL",
			`[{"tag_name":"v0.3.0","assets":[{"name":"` + ManifestName + `","browser_download_url":"https://evil.example/` + ManifestName + `"},{"name":"` + SignatureName + `","browser_download_url":"` + dl("v0.3.0", SignatureName) + `"}]}]`,
			false, ""},
		{"build metadata", `[{"tag_name":"v0.3.0+x","assets":[` + both("v0.3.0+x") + `]}]`, false, ""},
		{"nothing", `[]`, false, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, s, err := pickRelease([]byte(tt.listing), ReleasesURL, tt.test)
			if tt.want == "" {
				if CodeOf(err) != CodeNetwork {
					t.Errorf("pickRelease = %s, %s, %v; want none", m, s, err)
				}
				return
			}
			if err != nil || m != dl("v"+tt.want, ManifestName) || s != dl("v"+tt.want, SignatureName) {
				t.Errorf("pickRelease = %s, %s, %v; want %s", m, s, err, tt.want)
			}
		})
	}
}

// A 429 on the stable channel's download URLs is rate_limited too; other statuses are network.
func TestStableStatusCodes(t *testing.T) {
	for _, tt := range []struct {
		status int
		code   string
	}{{http.StatusTooManyRequests, CodeRateLimited}, {http.StatusForbidden, CodeNetwork}, {http.StatusNotFound, CodeNetwork}} {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t)
				h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					w.WriteHeader(tt.status)
					return true
				})
				h.checkNow(StateError, tt.code)
			})
		})
	}
}

// ISSHONI_UPDATE_TEST_BASE's endpoints (06 §8.11): the base replaces the releases URL and the API, and redirects stay
// under it.
func TestTestEndpoints(t *testing.T) {
	for _, tt := range []struct {
		base string
		ok   bool
	}{
		{"http://127.0.0.1:8123", true},
		{"https://127.0.0.1:8443/root", true},
		{"https://relserve.example", true},
		{"http://127.0.0.1", false},
		{"http://127.0.0.1:0", false},
		{"http://localhost:8123", false},
		{"http://10.0.0.1:8123", false},
		{"https://127.0.0.1:8443/", false},
		{"https://127.0.0.1:8443/a?b=c", false},
		{"https://127.0.0.1:8443/a#b", false},
		{"https://u@127.0.0.1:8443", false},
		{"https://127.0.0.1:8443/../a", false},
		{"ftp://127.0.0.1:21", false},
		{"", false},
	} {
		ep, err := testEndpoints(tt.base)
		if (err == nil) != tt.ok {
			t.Errorf("testEndpoints(%q) = %v, want ok %t", tt.base, err, tt.ok)
			continue
		}
		if !tt.ok {
			continue
		}
		if ep.releases != tt.base || ep.api != tt.base+"/api/releases" {
			t.Errorf("testEndpoints(%q) = %+v", tt.base, ep)
		}
	}
	ep, err := testEndpoints("http://127.0.0.1:8123/r")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"http://127.0.0.1:8123/r/download/v0.0.1-ci.2/x", true},
		{"http://127.0.0.1:8123/r", true},
		{"http://127.0.0.1:8123/rr/x", false},
		{"https://127.0.0.1:8123/r/x", false},
		{"http://127.0.0.1:8124/r/x", false},
		{"https://github.com/x", false},
	} {
		u, _ := url.Parse(tt.url)
		if err := ep.allowRedirect(u); (err == nil) != tt.ok {
			t.Errorf("under the test base, allowRedirect(%s) = %v, want ok %t", tt.url, err, tt.ok)
		}
	}
}

// An updater on a test base (the updatetest hooks' env) checks and downloads the updater e2e's ci manifests, as
// tools/desktoppkg manifest -releases writes them.
func TestTestBaseCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t)
		const base = "https://relserve.example/root" // the fake GitHub speaks TLS only
		installer := installerBytes(100, 4)
		h.gh.publish(h.active, "0.0.1-ci.2", installer, func(f map[string]any) {
			f["notesUrl"] = base + "/tag/v0.0.1-ci.2"
			f["artifacts"].([]any)[0].(map[string]any)["url"] = base + "/download/v0.0.1-ci.2/isshoni-desktop_0.0.1-ci.2_windows_amd64_setup.exe"
		})
		h.gh.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Host != "relserve.example" {
				return false
			}
			if name, ok := strings.CutPrefix(r.URL.Path, "/root/latest/download/"); ok {
				redirect(w, base+"/download/v0.0.1-ci.2/"+name)
				return true
			}
			if key, ok := strings.CutPrefix(r.URL.Path, "/root/download/"); ok && h.gh.asset(key) != nil {
				_, _ = w.Write(h.gh.asset(key))
				return true
			}
			return false
		})
		ep, err := testEndpoints(base)
		if err != nil {
			t.Fatal(err)
		}
		h.current = "0.0.1-ci.1"
		u, err := newUpdater(Options{CurrentVersion: h.current, DataDir: h.data,
			HTTP: &http.Client{Transport: h.gh.tr}, Now: h.now}, env{
			signers: func() ([]TrustedKey, error) { return keys(h.active, h.backup), nil },
			ep:      ep, testBuild: true,
			sys: system{goos: "windows", goarch: "amd64", installed: func() (bool, error) { return true, nil },
				freeSpace: func(string) (uint64, error) { return 1 << 40, nil }, policy: func() bool { return false },
				rename: os.Rename},
		})
		if err != nil {
			t.Fatal(err)
		}
		st, err := u.CheckNow(t.Context())
		if st.State != StateReady || st.Available != "0.0.1-ci.2" || err != nil {
			t.Fatalf("CheckNow on the test base = %+v, %v", st, err)
		}
		if p := u.Ready(); p == nil || p.Path != filepath.Join(h.dir, "isshoni-desktop_0.0.1-ci.2_windows_amd64_setup.exe") {
			t.Errorf("Ready() = %+v", p)
		}
		for _, r := range h.gh.requests() {
			if r.host != "relserve.example" {
				t.Errorf("a request outside the test base: %s", r)
			}
		}
	})
}

func TestReadSignersFile(t *testing.T) {
	a, b := newTestSigner(t, "isshoni-update-1"), newTestSigner(t, "isshoni-update-backup-1")
	file := filepath.Join(t.TempDir(), "update_signers")
	data := "isshoni-update namespaces=\"isshoni-update\" " + a.authorizedKey() + " " + a.Name + "\r\n" +
		"isshoni-update namespaces=\"isshoni-update\" " + b.authorizedKey() + " " + b.Name + "\r\n"
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSignersFile(file)
	if err != nil || len(got) != 2 || got[0].Backup || !got[1].Backup {
		t.Errorf("readSignersFile = %+v, %v", got, err)
	}
	if err := os.WriteFile(file, bytes.Repeat([]byte("#"), MaxManifestSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSignersFile(file); err == nil {
		t.Error("an oversize signers file was read")
	}
	if _, err := readSignersFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing signers file was read")
	}
}
