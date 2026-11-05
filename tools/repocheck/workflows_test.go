package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The workflow checks of docs/m2/06-windows-release.md §10.4–§10.5 (README WA75): no workflow that can reach the
// owner's PC (a self-hosted runner) is ever triggered by a pull request, and audiotest.yml keeps the guard, the
// labels, the approval environment and the token rules the spec gives it. They read the repository's own files, so
// they run in `task test:go` (tools/) and fail a change that breaks them.

// repoRoot is the repository root, two levels above this package (tools/repocheck).
const repoRoot = "../.."

// workflow is the part of a workflow file the checks read.
type workflow struct {
	On          yaml.Node      `yaml:"on"`
	Permissions yaml.Node      `yaml:"permissions"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	If          string    `yaml:"if"`
	Needs       yaml.Node `yaml:"needs"`
	RunsOn      yaml.Node `yaml:"runs-on"`
	Environment yaml.Node `yaml:"environment"`
	Permissions yaml.Node `yaml:"permissions"`
	Steps       []step    `yaml:"steps"`
	Strategy    struct {
		Matrix yaml.Node `yaml:"matrix"`
	} `yaml:"strategy"`
}

type step struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
	Env  map[string]string `yaml:"env"`
}

func parseWorkflow(data []byte) (workflow, error) {
	var w workflow
	if err := yaml.Unmarshal(data, &w); err != nil {
		return workflow{}, err
	}
	if w.On.Kind == 0 {
		return workflow{}, errors.New("no on: key")
	}
	return w, nil
}

// triggers lists the events of a workflow's on: key in any of its three forms (a string, a list, a mapping).
func triggers(on yaml.Node) ([]string, error) {
	switch on.Kind {
	case yaml.ScalarNode:
		return []string{on.Value}, nil
	case yaml.SequenceNode:
		var out []string
		for _, n := range on.Content {
			if n.Kind != yaml.ScalarNode {
				return nil, fmt.Errorf("line %d: an on: list entry is not an event name", n.Line)
			}
			out = append(out, n.Value)
		}
		return out, nil
	case yaml.MappingNode:
		var out []string
		for i := 0; i+1 < len(on.Content); i += 2 {
			out = append(out, on.Content[i].Value)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("line %d: on: is neither an event, a list nor a mapping", on.Line)
	}
}

// dispatchOnly fails unless workflow_dispatch is the workflow's only trigger.
func dispatchOnly(w workflow) error {
	events, err := triggers(w.On)
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0] != "workflow_dispatch" {
		return fmt.Errorf("on: has %v, want only workflow_dispatch (never pull_request, pull_request_target or workflow_run)", events)
	}
	return nil
}

// runsOnLabels is a job's runs-on as a list of labels (a string, a list, or the {group, labels} form).
func runsOnLabels(n yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		for _, c := range n.Content {
			out = append(out, c.Value)
		}
		return out
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == "labels" {
				return runsOnLabels(*n.Content[i+1])
			}
		}
	}
	return nil
}

// selfHosted reports whether a job may run on a self-hosted runner: its runs-on names self-hosted, or holds an
// expression that could name it. A label that is exactly ${{ matrix.<key> }} is resolved from the job's matrix
// (the key's values and its include entries); any other expression counts as self-hosted.
func selfHosted(j job) bool {
	for _, l := range runsOnLabels(j.RunsOn) {
		if strings.Contains(l, "self-hosted") {
			return true
		}
		if !strings.Contains(l, "${{") {
			continue
		}
		key, ok := matrixKey(l)
		if !ok {
			return true
		}
		values, ok := matrixValues(j.Strategy.Matrix, key)
		if !ok || slices.Contains(values, "self-hosted") {
			return true
		}
	}
	return false
}

// matrixKey returns KEY for a label that is exactly "${{ matrix.KEY }}".
func matrixKey(label string) (string, bool) {
	inner, ok := strings.CutPrefix(strings.TrimSpace(label), "${{")
	if !ok {
		return "", false
	}
	inner, ok = strings.CutSuffix(inner, "}}")
	if !ok {
		return "", false
	}
	key, ok := strings.CutPrefix(strings.TrimSpace(inner), "matrix.")
	if !ok || key == "" || strings.ContainsAny(key, " .[(") {
		return "", false
	}
	return key, true
}

// matrixValues lists every scalar a matrix gives key (its list and its include entries); ok is false when the
// matrix can't be read statically (an expression).
func matrixValues(matrix yaml.Node, key string) (values []string, ok bool) {
	if matrix.Kind != yaml.MappingNode {
		return nil, false
	}
	var scalars func(n *yaml.Node) bool
	scalars = func(n *yaml.Node) bool {
		switch n.Kind {
		case yaml.ScalarNode:
			if strings.Contains(n.Value, "${{") {
				return false
			}
			values = append(values, n.Value)
			return true
		case yaml.SequenceNode, yaml.MappingNode:
			step := 1
			start := 0
			if n.Kind == yaml.MappingNode {
				step, start = 2, 1 // the values of a mapping (a runs-on {group, labels})
			}
			for i := start; i < len(n.Content); i += step {
				if !scalars(n.Content[i]) {
					return false
				}
			}
			return true
		}
		return false
	}
	for i := 0; i+1 < len(matrix.Content); i += 2 {
		k, v := matrix.Content[i].Value, matrix.Content[i+1]
		switch k {
		case key:
			if !scalars(v) {
				return nil, false
			}
		case "include":
			if v.Kind != yaml.SequenceNode {
				return nil, false
			}
			for _, entry := range v.Content {
				if entry.Kind != yaml.MappingNode {
					return nil, false
				}
				for e := 0; e+1 < len(entry.Content); e += 2 {
					if entry.Content[e].Value == key && !scalars(entry.Content[e+1]) {
						return nil, false
					}
				}
			}
		}
	}
	return values, true
}

// permissionMap is a permissions: mapping (scope → level); ok is false for the read-all/write-all strings.
func permissionMap(n yaml.Node) (perms map[string]string, set, ok bool) {
	switch n.Kind {
	case 0:
		return nil, false, true
	case yaml.MappingNode:
		perms = map[string]string{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			perms[n.Content[i].Value] = n.Content[i+1].Value
		}
		return perms, true, true
	default:
		return nil, true, false
	}
}

// checkNoPRToSelfHosted is the rule for every workflow: one that has a job for a self-hosted runner is
// dispatch-only (06 §10.4, README §4 "Branches and PRs").
func checkNoPRToSelfHosted(w workflow) error {
	var jobs []string
	for name, j := range w.Jobs {
		if selfHosted(j) {
			jobs = append(jobs, name)
		}
	}
	if len(jobs) == 0 {
		return nil
	}
	sort.Strings(jobs)
	if err := dispatchOnly(w); err != nil {
		return fmt.Errorf("jobs %v can run on a self-hosted runner: %w", jobs, err)
	}
	return nil
}

// tokenUse finds a reference to a token or a secret in a step.
func tokenUse(s step) string {
	texts := []string{s.Run}
	for _, v := range s.With {
		texts = append(texts, v)
	}
	for _, v := range s.Env {
		texts = append(texts, v)
	}
	for _, t := range texts {
		for _, bad := range []string{"github.token", "secrets.", "GITHUB_TOKEN", "GH_TOKEN"} {
			if strings.Contains(t, bad) {
				return bad
			}
		}
	}
	return ""
}

// checkAudiotest checks audiotest.yml against 06 §10.4 and README §3 item 10.
func checkAudiotest(w workflow) error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if err := dispatchOnly(w); err != nil {
		fail("%v", err)
	} else {
		var on struct {
			WorkflowDispatch struct {
				Inputs map[string]struct {
					Type     string   `yaml:"type"`
					Options  []string `yaml:"options"`
					Required bool     `yaml:"required"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		}
		if err := w.On.Decode(&on); err != nil {
			fail("on.workflow_dispatch: %v", err)
		}
		in := on.WorkflowDispatch.Inputs
		if o, ok := in["os"]; !ok || o.Type != "choice" || !slices.Equal(o.Options, []string{"win11", "win10"}) || !o.Required {
			fail("input os must be a required choice of win11, win10")
		}
		if s, ok := in["suite"]; !ok || s.Type != "choice" || !slices.Equal(s.Options, []string{"release", "full", "hwcheck"}) {
			fail("input suite must be a choice of release, full, hwcheck")
		}
		if r, ok := in["release_run_id"]; !ok || r.Type != "string" || r.Required {
			fail("input release_run_id must be an optional string")
		}
	}

	if perms, set, ok := permissionMap(w.Permissions); !ok || !set || len(perms) != 1 || perms["contents"] != "read" {
		fail("the workflow's permissions must be exactly {contents: read}")
	}

	for name, j := range w.Jobs {
		perms, _, ok := permissionMap(j.Permissions)
		if !ok {
			fail("job %s: permissions must be a mapping, not read-all or write-all", name)
			continue
		}
		for scope, level := range perms {
			if level == "write" {
				fail("job %s: %s: write; no job of audiotest.yml writes", name, scope)
			}
			if scope == "actions" && name != "fetch" {
				fail("job %s has actions: %s; fetch is the only job with actions: read", name, level)
			}
		}
	}

	fetch, ok := w.Jobs["fetch"]
	if !ok {
		fail("no fetch job")
	} else {
		perms, _, _ := permissionMap(fetch.Permissions)
		if perms["actions"] != "read" {
			fail("fetch must have actions: read (it downloads the release.yml run's isshoni-exit-tools)")
		}
		if !strings.Contains(fetch.If, "inputs.release_run_id != ''") {
			fail("fetch must run only with release_run_id (its if: is %q)", fetch.If)
		}
		if selfHosted(fetch) {
			fail("fetch must run on a GitHub-hosted runner: the token that reads another run stays there")
		}
		downloads := false
		for _, s := range fetch.Steps {
			if strings.HasPrefix(s.Uses, "actions/download-artifact@") && s.With["name"] == "isshoni-exit-tools" &&
				s.With["run-id"] == "${{ inputs.release_run_id }}" {
				downloads = true
			}
		}
		if !downloads {
			fail("fetch must download isshoni-exit-tools with run-id ${{ inputs.release_run_id }}")
		}
	}

	if b, ok := w.Jobs["build"]; !ok {
		fail("no build job")
	} else if !strings.Contains(b.If, "inputs.release_run_id == ''") || selfHosted(b) {
		fail("build must run on a GitHub-hosted runner and only without release_run_id")
	}

	run, ok := w.Jobs["run"]
	if !ok {
		fail("no run job")
		return errors.Join(errs...)
	}
	for _, guard := range []string{
		"github.repository == 'MoonWX/isshoni'",
		"github.actor == github.repository_owner",
		"needs.build.result == 'success' || needs.fetch.result == 'success'",
	} {
		if !strings.Contains(run.If, guard) {
			fail("run's if: lacks %q", guard)
		}
	}
	var needs []string
	if err := run.Needs.Decode(&needs); err != nil || !slices.Contains(needs, "build") || !slices.Contains(needs, "fetch") {
		fail("run must need build and fetch")
	}
	wantLabels := []string{"self-hosted", "windows", "x64", "isshoni-audio", "${{ inputs.os }}"}
	if got := runsOnLabels(run.RunsOn); !slices.Equal(got, wantLabels) {
		fail("run's runs-on is %v, want %v (README §3 item 10)", got, wantLabels)
	}
	if run.Environment.Kind != yaml.ScalarNode || run.Environment.Value != "audiotest" {
		fail("run's environment must be audiotest (the owner's approval, 05 §18.8)")
	}
	if perms, set, _ := permissionMap(run.Permissions); !set || len(perms) != 0 {
		fail("run's permissions must be {}: it uses no token")
	}
	for _, s := range run.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			fail("run step %q checks out: run has no checkout", s.Name)
		}
		if bad := tokenUse(s); bad != "" {
			fail("run step %q uses %s: run uses no token or secret", s.Name, bad)
		}
		if strings.HasPrefix(s.Uses, "actions/download-artifact@") && (s.With["run-id"] != "" || s.With["github-token"] != "") {
			fail("run step %q downloads from another run: only fetch does", s.Name)
		}
	}
	return errors.Join(errs...)
}

func readWorkflow(t *testing.T, path string) workflow {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	w, err := parseWorkflow(data)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return w
}

// TestAudiotestWorkflow reads .github/workflows/audiotest.yml: dispatch-only, the guard, the labels, the
// environment, fetch as the only job with actions: read, and no checkout or token in run.
func TestAudiotestWorkflow(t *testing.T) {
	w := readWorkflow(t, filepath.Join(repoRoot, ".github", "workflows", "audiotest.yml"))
	if err := checkAudiotest(w); err != nil {
		t.Errorf("audiotest.yml:\n%v", err)
	}
}

// TestNoPullRequestToSelfHosted reads every workflow: one with a job that can run on a self-hosted runner has only
// the workflow_dispatch trigger.
func TestNoPullRequestToSelfHosted(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflows found")
	}
	selfHostedSeen := false
	for _, f := range files {
		w := readWorkflow(t, f)
		for _, j := range w.Jobs {
			selfHostedSeen = selfHostedSeen || selfHosted(j)
		}
		if err := checkNoPRToSelfHosted(w); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
	if !selfHostedSeen {
		t.Error("no workflow has a self-hosted job: the check would pass vacuously (audiotest.yml's run job has one)")
	}
}

// goodAudiotest is a minimal audiotest.yml that passes checkAudiotest; the cases below break one rule each.
const goodAudiotest = `on:
  workflow_dispatch:
    inputs:
      os: {type: choice, options: [win11, win10], required: true}
      suite: {type: choice, options: [release, full, hwcheck], default: release}
      release_run_id: {type: string, required: false}
permissions: {contents: read}
jobs:
  build:
    if: inputs.release_run_id == ''
    runs-on: windows-2025
    permissions: {contents: read}
    steps:
      - uses: actions/checkout@0000000000000000000000000000000000000000
  fetch:
    if: inputs.release_run_id != '' && github.repository == 'MoonWX/isshoni'
    runs-on: ubuntu-24.04
    permissions: {actions: read}
    steps:
      - run: gh api repos/x
        env: {GH_TOKEN: "${{ github.token }}"}
      - uses: actions/download-artifact@0000000000000000000000000000000000000000
        with:
          name: isshoni-exit-tools
          run-id: ${{ inputs.release_run_id }}
          github-token: ${{ github.token }}
  run:
    needs: [build, fetch]
    if: >-
      !cancelled() && github.repository == 'MoonWX/isshoni' && github.actor == github.repository_owner &&
      (needs.build.result == 'success' || needs.fetch.result == 'success')
    runs-on: [self-hosted, windows, x64, isshoni-audio, "${{ inputs.os }}"]
    environment: audiotest
    permissions: {}
    steps:
      - uses: actions/download-artifact@0000000000000000000000000000000000000000
        with: {name: audiotest-tools}
      - run: .\isshoni-audiotest.exe run -gate -out .
`

func TestCheckAudiotest(t *testing.T) {
	cases := []struct {
		name    string
		old     string
		new     string
		wantErr string // "" = passes
	}{
		{name: "good"},
		{"pull_request", "on:\n  workflow_dispatch:", "on:\n  pull_request:\n  workflow_dispatch:", "want only workflow_dispatch"},
		{"pull_request_target", "on:\n  workflow_dispatch:", "on:\n  pull_request_target:\n  workflow_dispatch:", "want only workflow_dispatch"},
		{"workflow_run", "on:\n  workflow_dispatch:", "on:\n  workflow_run: {workflows: [CI]}\n  workflow_dispatch:", "want only workflow_dispatch"},
		{"no repository guard", "github.repository == 'MoonWX/isshoni' && github.actor", "github.actor", "github.repository == 'MoonWX/isshoni'"},
		{"no actor guard", "github.actor == github.repository_owner &&", "", "github.actor == github.repository_owner"},
		{"labels", "isshoni-audio, ", "", "run's runs-on"},
		{"no environment", "    environment: audiotest\n", "", "environment must be audiotest"},
		{"checkout in run", "      - run: .\\isshoni", "      - uses: actions/checkout@0000000000000000000000000000000000000000\n      - run: .\\isshoni", "checks out"},
		{"token in run", "-gate -out .", "-gate -out . ${{ github.token }}", "uses github.token"},
		{"secret in run", "-gate -out .", "-gate -out . ${{ secrets.X }}", "uses secrets."},
		{"run permissions", "    permissions: {}\n    steps:\n      - uses: actions/download", "    permissions: {contents: read}\n    steps:\n      - uses: actions/download", "permissions must be {}"},
		{"actions read in build", "    runs-on: windows-2025\n    permissions: {contents: read}", "    runs-on: windows-2025\n    permissions: {contents: read, actions: read}", "only job with actions: read"},
		{"fetch without actions read", "permissions: {actions: read}", "permissions: {contents: read}", "fetch must have actions: read"},
		{"fetch self-hosted", "    runs-on: ubuntu-24.04", "    runs-on: [self-hosted, linux]", "GitHub-hosted"},
		{"no release_run_id", "      release_run_id: {type: string, required: false}\n", "", "release_run_id must be an optional string"},
		{"write permission", "permissions: {actions: read}", "permissions: {actions: read, contents: write}", "no job of audiotest.yml writes"},
		{"read-all", "permissions: {contents: read}\njobs:", "permissions: read-all\njobs:", "exactly {contents: read}"},
		{"run downloads another run", "with: {name: audiotest-tools}", "with: {name: audiotest-tools, run-id: \"1\"}", "downloads from another run"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := goodAudiotest
			if tc.old != "" {
				if !strings.Contains(src, tc.old) {
					t.Fatalf("the fixture has no %q", tc.old)
				}
				src = strings.Replace(src, tc.old, tc.new, 1)
			}
			w, err := parseWorkflow([]byte(src))
			if err != nil {
				t.Fatal(err)
			}
			err = checkAudiotest(w)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("want no error, got:\n%v", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("want an error with %q, got none", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("want an error with %q, got:\n%v", tc.wantErr, err)
			}
		})
	}
}

func TestCheckNoPRToSelfHosted(t *testing.T) {
	cases := []struct {
		name string
		src  string
		ok   bool
	}{
		{"hosted with pull_request", "on: [pull_request, push]\njobs:\n  a: {runs-on: ubuntu-24.04}\n", true},
		{"self-hosted, dispatch", "on: {workflow_dispatch: {}}\njobs:\n  a: {runs-on: [self-hosted, windows]}\n", true},
		{"self-hosted, string pull_request", "on: pull_request\njobs:\n  a: {runs-on: [self-hosted, windows]}\n", false},
		{"self-hosted, list", "on: [workflow_dispatch, pull_request_target]\njobs:\n  a: {runs-on: self-hosted}\n", false},
		{"self-hosted in a group's labels", "on: {workflow_run: {}}\njobs:\n  a: {runs-on: {group: g, labels: [self-hosted]}}\n", false},
		{"input runs-on", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ inputs.runner }}\"}\n", false},
		{"hosted matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [ubuntu-24.04, macos-15]}}}\n", true},
		{"self-hosted matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [ubuntu-24.04, self-hosted]}}}\n", false},
		{"self-hosted matrix include", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.r }}\", strategy: {matrix: {include: [{r: ubuntu-24.04}, {r: [self-hosted, x64]}]}}}\n", false},
		{"matrix without the key", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {go: [a, b]}}}\n", true},
		{"matrix from JSON", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: \"${{ fromJSON(inputs.m) }}\"}}\n", false},
		{"matrix value from an expression", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [\"${{ inputs.r }}\"]}}}\n", false},
		{"quoted on key", "\"on\": {push: {}, workflow_dispatch: {}}\njobs:\n  a: {runs-on: [self-hosted]}\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := parseWorkflow([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			err = checkNoPRToSelfHosted(w)
			if tc.ok && err != nil {
				t.Errorf("want no error, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}
