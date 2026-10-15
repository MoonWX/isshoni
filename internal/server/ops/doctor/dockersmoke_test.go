package doctor_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
)

// TestDockerSmokeExpectations holds the doctor step of deploy/docker/test/docker-smoke.sh (docs/m1/06-deploy-and-ci.md
// §11.3, README F21) to this package's checks and codes. The step asserts every check of `isshoni doctor
// --list-checks` by its code, from two lists in the script: doctor_want (one code per check) and doctor_allowed
// (the codes an engine may give instead). So the step depends on this package, while CI's docker-smoke job runs on
// a pull request only when its Docker, deploy or release paths change (06 §8.2).
//
// This test runs in CI's test-go job on every Go change. A pull request that adds a check, removes one, or renames
// or removes a code that the lists name fails here until the script knows it, and that edit of deploy/docker/**
// runs docker-smoke on the same pull request. Which of its codes a check gives in the smoke's setup only the step
// itself can tell.
func TestDockerSmokeExpectations(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "deploy", "docker", "test", "docker-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	ids := doctor.CheckIDs()
	codes := doctor.Codes()
	// A code the step names must be one doctor has, and one of that check: <id>.<what>, or a skip code.
	checkCode := func(list, id, code string) {
		t.Helper()
		switch {
		case !slices.Contains(codes, code):
			t.Errorf("%s: %s %s: doctor has no code %q", list, id, code, code)
		case !strings.HasPrefix(code, id+".") && !strings.HasPrefix(code, "skip."):
			t.Errorf("%s: %s %s: the code is another check's", list, id, code)
		}
	}

	t.Run("doctor_want", func(t *testing.T) {
		var listed []string
		for _, line := range heredoc(t, script, "doctor_want") {
			f := strings.Fields(line)
			if len(f) != 2 {
				t.Errorf("doctor_want: %q is not CHECK CODE", line)
				continue
			}
			listed = append(listed, f[0])
			checkCode("doctor_want", f[0], f[1])
		}
		// One line per check, in the order of `isshoni doctor --list-checks`, as the script says.
		if !slices.Equal(listed, ids) {
			t.Errorf("doctor_want lists the checks\n  %v\ndoctor has (doctor --list-checks)\n  %v", listed, ids)
		}
	})

	t.Run("doctor_allowed", func(t *testing.T) {
		engines := map[string][]string{} // check id → the engines its entries name
		for _, line := range heredoc(t, script, "doctor_allowed") {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			f := strings.Fields(line)
			if len(f) != 3 {
				t.Errorf("doctor_allowed: %q is not CHECK ENGINE CODES", line)
				continue
			}
			id, engine := f[0], f[1]
			if !slices.Contains(ids, id) {
				t.Errorf("doctor_allowed: %q: doctor has no check %q", line, id)
			}
			if !slices.Contains([]string{"local", "vm", "any"}, engine) {
				t.Errorf("doctor_allowed: %q: the engine is local, vm or any", line)
			}
			// The script takes the first entry for the check that names this engine or any: a second one would
			// never count.
			for _, seen := range engines[id] {
				if seen == engine || seen == "any" || engine == "any" {
					t.Errorf("doctor_allowed: %q: %s already has an entry for %s", line, id, seen)
				}
			}
			engines[id] = append(engines[id], engine)
			for code := range strings.SplitSeq(f[2], "|") {
				checkCode("doctor_allowed", id, code)
			}
		}
		if len(engines) == 0 {
			t.Error("doctor_allowed has no entry")
		}
	})
}

// heredoc returns the lines of the here-document that the shell function name prints: the function's first
// `<<'EOF'` up to its EOF line.
func heredoc(t *testing.T, script, name string) []string {
	t.Helper()
	lines := strings.Split(script, "\n")
	start := slices.Index(lines, name+"() {")
	if start < 0 {
		t.Fatalf("docker-smoke.sh has no function %s", name)
	}
	for i := start + 1; i < len(lines) && lines[i] != "}"; i++ {
		if !strings.HasSuffix(lines[i], "<<'EOF'") {
			continue
		}
		end := slices.Index(lines[i+1:], "EOF")
		if end < 0 {
			break
		}
		return lines[i+1 : i+1+end]
	}
	t.Fatalf("docker-smoke.sh: %s prints no here-document <<'EOF' ... EOF", name)
	return nil
}
