package main

import (
	"errors"
	"fmt"
	"os"
	"path"
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
// they run in `task test:go` (tools/), and in CI's lint-deploy job on every pull request that changes
// .github/workflows/** or .github/actionlint.yaml (README WF15), and fail a change that breaks them.
//
// A job counts as self-hosted (README WF15) when its runs-on names the literal self-hosted label, names a runner
// group, uses a label registered for a self-hosted runner, or holds an expression that could resolve to any of
// these. The registered labels are actionlint's self-hosted-runner labels in .github/actionlint.yaml: actionlint
// (lint:actions) rejects a runs-on label that is neither GitHub-hosted nor listed there, so every custom label a
// workflow can use is in that one list, and repocheck reads the same list instead of keeping a copy.

// repoRoot is the repository root, two levels above this package (tools/repocheck).
const repoRoot = "../.."

// audioRunnerLabels are the labels of the owner's self-hosted audio runner (06 §10.5). .github/actionlint.yaml must
// register each of them, so that a job naming one counts as self-hosted even without the literal self-hosted.
var audioRunnerLabels = []string{"isshoni-audio", "win11", "win10"}

// workflow is the part of a workflow file the checks read.
type workflow struct {
	On          yaml.Node      `yaml:"on"`
	Permissions yaml.Node      `yaml:"permissions"`
	Env         yaml.Node      `yaml:"env"`
	Jobs        map[string]job `yaml:"jobs"`
}

type job struct {
	If          string    `yaml:"if"`
	Needs       yaml.Node `yaml:"needs"`
	RunsOn      yaml.Node `yaml:"runs-on"`
	Environment yaml.Node `yaml:"environment"`
	Permissions yaml.Node `yaml:"permissions"`
	Env         yaml.Node `yaml:"env"`
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

// runnerLabels are the label patterns registered for self-hosted runners: actionlint's self-hosted-runner labels,
// which may use path.Match's glob syntax as actionlint's do. GitHub compares labels case-insensitively.
type runnerLabels []string

// registered reports whether label matches one of the patterns.
func (r runnerLabels) registered(label string) bool {
	label = strings.ToLower(strings.TrimSpace(label))
	for _, p := range r {
		p = strings.ToLower(p)
		if ok, err := path.Match(p, label); p == label || (err == nil && ok) {
			return true
		}
	}
	return false
}

// selfHostedLabel reports whether one plain runs-on label (not an expression) picks a self-hosted runner: the
// literal self-hosted, or a registered label.
func (r runnerLabels) selfHostedLabel(label string) bool {
	return strings.Contains(strings.ToLower(label), "self-hosted") || r.registered(label)
}

// readRunnerLabels reads the self-hosted-runner labels of an actionlint configuration.
func readRunnerLabels(file string) (runnerLabels, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	return parseRunnerLabels(data)
}

// parseRunnerLabels reads the self-hosted-runner labels of an actionlint configuration; none is an error, since then
// only the literal self-hosted would count.
func parseRunnerLabels(data []byte) (runnerLabels, error) {
	var cfg struct {
		SelfHostedRunner struct {
			Labels []string `yaml:"labels"`
		} `yaml:"self-hosted-runner"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	var out runnerLabels
	for _, l := range cfg.SelfHostedRunner.Labels {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("self-hosted-runner.labels is empty")
	}
	return out, nil
}

// runsOn reads a job's runs-on in any of its forms: a label, a list of labels, or the {group, labels} mapping.
// group is true when it names a runner group, or when it has a shape that can't be read statically.
func runsOn(n yaml.Node) (labels []string, group bool) {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}, false
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return labels, true
			}
			labels = append(labels, c.Value)
		}
		return labels, false
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value != "labels" {
				group = true // group, or a key GitHub doesn't know: don't guess where it runs
				continue
			}
			l, g := runsOn(*n.Content[i+1])
			labels, group = append(labels, l...), group || g
		}
		return labels, group
	}
	return nil, false
}

// runsOnLabels is a job's runs-on as a list of labels.
func runsOnLabels(n yaml.Node) []string {
	labels, _ := runsOn(n)
	return labels
}

// selfHosted reports whether a job may run on a self-hosted runner: its runs-on names a runner group, the literal
// self-hosted or a registered self-hosted label, or holds an expression that could name one of them. A label that is
// exactly ${{ matrix.<key> }} is resolved from the job's matrix (the key's values and its include entries); any other
// expression counts as self-hosted.
func (r runnerLabels) selfHosted(j job) bool {
	labels, group := runsOn(j.RunsOn)
	if group {
		return true
	}
	for _, l := range labels {
		if !strings.Contains(l, "${{") {
			if r.selfHostedLabel(l) {
				return true
			}
			continue
		}
		key, ok := matrixKey(l)
		if !ok {
			return true
		}
		values, group, ok := matrixValues(j.Strategy.Matrix, key)
		if !ok || group || slices.ContainsFunc(values, r.selfHostedLabel) {
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

// matrixValues lists every label a matrix gives key (its list and its include entries). group is true when one of
// them is a runs-on mapping with a key other than labels (a runner group); ok is false when the matrix can't be read
// statically (an expression).
func matrixValues(matrix yaml.Node, key string) (values []string, group, ok bool) {
	if matrix.Kind != yaml.MappingNode {
		return nil, false, false
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
		case yaml.SequenceNode:
			for _, c := range n.Content {
				if !scalars(c) {
					return false
				}
			}
			return true
		case yaml.MappingNode: // a runs-on {group, labels}
			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value != "labels" {
					group = true
				}
				if !scalars(n.Content[i+1]) {
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
				return nil, false, false
			}
		case "include":
			if v.Kind != yaml.SequenceNode {
				return nil, false, false
			}
			for _, entry := range v.Content {
				if entry.Kind != yaml.MappingNode {
					return nil, false, false
				}
				for e := 0; e+1 < len(entry.Content); e += 2 {
					if entry.Content[e].Value == key && !scalars(entry.Content[e+1]) {
						return nil, false, false
					}
				}
			}
		}
	}
	return values, group, true
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
func checkNoPRToSelfHosted(w workflow, r runnerLabels) error {
	var jobs []string
	for name, j := range w.Jobs {
		if r.selfHosted(j) {
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
	return tokenIn(texts...)
}

// tokenIn finds a reference to a token or a secret in texts.
func tokenIn(texts ...string) string {
	for _, t := range texts {
		for _, bad := range []string{"github.token", "secrets.", "GITHUB_TOKEN", "GH_TOKEN"} {
			if strings.Contains(t, bad) {
				return bad
			}
		}
	}
	return ""
}

// envTokenUse finds a reference to a token or a secret in an env: block (a job's or the workflow's), in its names
// and its values. A block that is one expression (env: ${{ fromJSON(...) }}) is read as that text.
func envTokenUse(env yaml.Node) string {
	var texts []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.ScalarNode {
			texts = append(texts, n.Value)
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&env)
	return tokenIn(texts...)
}

// checkAudiotest checks audiotest.yml against 06 §10.4 and README §3 item 10.
func checkAudiotest(w workflow, r runnerLabels) error {
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
		if r.selfHosted(fetch) {
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
	} else if !strings.Contains(b.If, "inputs.release_run_id == ''") || r.selfHosted(b) {
		fail("build must run on a GitHub-hosted runner and only without release_run_id")
	}

	// The environment job is the only guard against GitHub creating an unprotected audiotest environment.
	if e, ok := w.Jobs["environment"]; !ok {
		fail("no environment job (it checks that audiotest has the owner as required reviewer)")
	} else {
		if r.selfHosted(e) {
			fail("environment must run on a GitHub-hosted runner")
		}
		if perms, _, _ := permissionMap(e.Permissions); perms["actions"] != "" {
			fail("environment must not have actions permission: fetch is the only job with actions: read")
		}
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
		"needs.environment.result == 'success'",
	} {
		if !strings.Contains(run.If, guard) {
			fail("run's if: lacks %q", guard)
		}
	}
	var needs []string
	if err := run.Needs.Decode(&needs); err != nil || !slices.Contains(needs, "build") || !slices.Contains(needs, "fetch") ||
		!slices.Contains(needs, "environment") {
		fail("run must need environment, build and fetch")
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
	// run's steps see the workflow's env and run's own, so neither may hand them a token.
	if bad := envTokenUse(w.Env); bad != "" {
		fail("the workflow's env uses %s: run uses no token or secret", bad)
	}
	if bad := envTokenUse(run.Env); bad != "" {
		fail("run's env uses %s: run uses no token or secret", bad)
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

// repoRunnerLabels reads the repository's registered self-hosted labels from .github/actionlint.yaml.
func repoRunnerLabels(t *testing.T) runnerLabels {
	t.Helper()
	r, err := readRunnerLabels(filepath.Join(repoRoot, ".github", "actionlint.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestActionlintRunnerLabels reads .github/actionlint.yaml: it registers the audio runner's labels (06 §10.5), so
// that both actionlint and these checks know them.
func TestActionlintRunnerLabels(t *testing.T) {
	r := repoRunnerLabels(t)
	for _, l := range audioRunnerLabels {
		if !r.registered(l) {
			t.Errorf(".github/actionlint.yaml's self-hosted-runner.labels %v lacks %s (06 §10.5)", []string(r), l)
		}
	}
}

// TestAudiotestWorkflow reads .github/workflows/audiotest.yml: dispatch-only, the guard, the labels, the
// environment, fetch as the only job with actions: read, and no checkout or token in run.
func TestAudiotestWorkflow(t *testing.T) {
	w := readWorkflow(t, filepath.Join(repoRoot, ".github", "workflows", "audiotest.yml"))
	if err := checkAudiotest(w, repoRunnerLabels(t)); err != nil {
		t.Errorf("audiotest.yml:\n%v", err)
	}
}

// TestNoPullRequestToSelfHosted reads every workflow: one with a job that can run on a self-hosted runner has only
// the workflow_dispatch trigger.
func TestNoPullRequestToSelfHosted(t *testing.T) {
	r := repoRunnerLabels(t)
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
			selfHostedSeen = selfHostedSeen || r.selfHosted(j)
		}
		if err := checkNoPRToSelfHosted(w, r); err != nil {
			t.Errorf("%s: %v", filepath.Base(f), err)
		}
	}
	if !selfHostedSeen {
		t.Error("no workflow has a self-hosted job: the check would pass vacuously (audiotest.yml's run job has one)")
	}

	// audiotest.yml's run job without the literal self-hosted still counts: its other labels are registered.
	w := readWorkflow(t, filepath.Join(repoRoot, ".github", "workflows", "audiotest.yml"))
	run := w.Jobs["run"]
	var labels []string
	for _, l := range runsOnLabels(run.RunsOn) {
		if l != "self-hosted" && !strings.Contains(l, "${{") {
			labels = append(labels, l)
		}
	}
	if err := run.RunsOn.Encode(labels); err != nil {
		t.Fatal(err)
	}
	if !r.selfHosted(run) {
		t.Errorf("audiotest.yml's run job on %v is not seen as self-hosted", labels)
	}
}

func TestRunnerLabels(t *testing.T) {
	if _, err := parseRunnerLabels([]byte("self-hosted-runner:\n  labels: []\n")); err == nil {
		t.Error("an empty label list: want an error, got none")
	}
	if _, err := parseRunnerLabels([]byte("paths: {}\n")); err == nil {
		t.Error("no self-hosted-runner key: want an error, got none")
	}
	r, err := parseRunnerLabels([]byte("self-hosted-runner:\n  labels:\n    - isshoni-audio\n    - gpu-*\n"))
	if err != nil {
		t.Fatal(err)
	}
	for label, want := range map[string]bool{
		"isshoni-audio": true,
		"ISSHONI-Audio": true, // GitHub's labels are case-insensitive
		"gpu-a100":      true, // actionlint's labels are glob patterns
		"gpu":           false,
		"isshoni":       false,
		"ubuntu-24.04":  false,
		"windows":       false,
	} {
		if got := r.registered(label); got != want {
			t.Errorf("registered(%q) = %v, want %v", label, got, want)
		}
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
env:
  ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}
jobs:
  environment:
    if: github.repository == 'MoonWX/isshoni' && github.actor == github.repository_owner
    runs-on: ubuntu-24.04
    permissions: {contents: read}
    steps:
      - run: gh api repos/x/environments/audiotest
        env: {GH_TOKEN: "${{ github.token }}"}
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
    needs: [environment, build, fetch]
    if: >-
      !cancelled() && github.repository == 'MoonWX/isshoni' && github.actor == github.repository_owner &&
      needs.environment.result == 'success' && (needs.build.result == 'success' || needs.fetch.result == 'success')
    runs-on: [self-hosted, windows, x64, isshoni-audio, "${{ inputs.os }}"]
    environment: audiotest
    permissions: {}
    env:
      SUITE: ${{ inputs.suite }}
    steps:
      - uses: actions/download-artifact@0000000000000000000000000000000000000000
        with: {name: audiotest-tools}
      - run: .\isshoni-audiotest.exe run -gate -out .
`

func TestCheckAudiotest(t *testing.T) {
	const (
		fetchRunsOn = "    runs-on: ubuntu-24.04\n    permissions: {actions: read}"
		envRunsOn   = "    runs-on: ubuntu-24.04\n    permissions: {contents: read}"
	)
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
		{"no repository guard", "!cancelled() && github.repository == 'MoonWX/isshoni' && github.actor", "!cancelled() && github.actor", "github.repository == 'MoonWX/isshoni'"},
		{"no actor guard", "github.actor == github.repository_owner &&", "", "github.actor == github.repository_owner"},
		{"labels", "isshoni-audio, ", "", "run's runs-on"},
		{"no environment", "    environment: audiotest\n", "", "environment must be audiotest"},
		{"checkout in run", "      - run: .\\isshoni", "      - uses: actions/checkout@0000000000000000000000000000000000000000\n      - run: .\\isshoni", "checks out"},
		{"token in run", "-gate -out .", "-gate -out . ${{ github.token }}", "uses github.token"},
		{"secret in run", "-gate -out .", "-gate -out . ${{ secrets.X }}", "uses secrets."},
		{"token in run's env", "      SUITE: ${{ inputs.suite }}\n", "      SUITE: ${{ inputs.suite }}\n      T: ${{ github.token }}\n", "run's env uses github.token"},
		{"secret in run's env", "      SUITE: ${{ inputs.suite }}\n", "      SUITE: ${{ inputs.suite }}\n      K: ${{ secrets.KEY }}\n", "run's env uses secrets."},
		{"GH_TOKEN named in run's env", "      SUITE: ${{ inputs.suite }}\n", "      SUITE: ${{ inputs.suite }}\n      GH_TOKEN: x\n", "run's env uses GH_TOKEN"},
		{"run's env from an expression", "    env:\n      SUITE: ${{ inputs.suite }}\n", "    env: ${{ fromJSON(secrets.ENV) }}\n", "run's env uses secrets."},
		{"token in the workflow's env", "  ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}\n", "  ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}\n  T: ${{ github.token }}\n", "the workflow's env uses github.token"},
		{"secret in the workflow's env", "  ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}\n", "  ISSHONI_VERSION: 0.0.0-ci.${{ github.run_number }}\n  K: ${{ secrets.KEY }}\n", "the workflow's env uses secrets."},
		{"run permissions", "    permissions: {}\n    env:", "    permissions: {contents: read}\n    env:", "permissions must be {}"},
		{"actions read in build", "    runs-on: windows-2025\n    permissions: {contents: read}", "    runs-on: windows-2025\n    permissions: {contents: read, actions: read}", "only job with actions: read"},
		{"fetch without actions read", "permissions: {actions: read}", "permissions: {contents: read}", "fetch must have actions: read"},
		{"fetch self-hosted", fetchRunsOn, "    runs-on: [self-hosted, linux]\n    permissions: {actions: read}", "fetch must run on a GitHub-hosted runner"},
		{"fetch on a registered label", fetchRunsOn, "    runs-on: [windows, isshoni-audio]\n    permissions: {actions: read}", "fetch must run on a GitHub-hosted runner"},
		{"fetch in a runner group", fetchRunsOn, "    runs-on: {group: audio}\n    permissions: {actions: read}", "fetch must run on a GitHub-hosted runner"},
		{"build on a registered label", "    runs-on: windows-2025\n", "    runs-on: win10\n", "build must run on a GitHub-hosted runner"},
		{"no release_run_id", "      release_run_id: {type: string, required: false}\n", "", "release_run_id must be an optional string"},
		{"write permission", "permissions: {actions: read}", "permissions: {actions: read, contents: write}", "no job of audiotest.yml writes"},
		{"read-all", "permissions: {contents: read}\nenv:", "permissions: read-all\nenv:", "exactly {contents: read}"},
		{"run doesn't need environment", "needs: [environment, build, fetch]", "needs: [build, fetch]", "run must need environment"},
		{"run's if lacks the environment result", "needs.environment.result == 'success' && ", "", "needs.environment.result == 'success'"},
		{"no environment job", "  environment:\n    if:", "  envcheck:\n    if:", "no environment job"},
		{"environment self-hosted", envRunsOn, "    runs-on: [self-hosted, linux]\n    permissions: {contents: read}", "environment must run on a GitHub-hosted runner"},
		{"environment on a registered label", envRunsOn, "    runs-on: [win11]\n    permissions: {contents: read}", "environment must run on a GitHub-hosted runner"},
		{"environment in a runner group", envRunsOn, "    runs-on: {group: g, labels: [ubuntu-24.04]}\n    permissions: {contents: read}", "environment must run on a GitHub-hosted runner"},
		{"environment with actions read", envRunsOn, "    runs-on: ubuntu-24.04\n    permissions: {contents: read, actions: read}", "environment must not have actions"},
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
			err = checkAudiotest(w, runnerLabels(audioRunnerLabels))
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
		{"hosted labels mapping", "on: {pull_request: {}}\njobs:\n  a: {runs-on: {labels: [ubuntu-24.04]}}\n", true},
		{"self-hosted, dispatch", "on: {workflow_dispatch: {}}\njobs:\n  a: {runs-on: [self-hosted, windows]}\n", true},
		{"registered label, dispatch", "on: {workflow_dispatch: {}}\njobs:\n  a: {runs-on: [windows, isshoni-audio]}\n", true},
		{"runner group, dispatch", "on: {workflow_dispatch: {}}\njobs:\n  a: {runs-on: {group: audio}}\n", true},
		{"self-hosted, string pull_request", "on: pull_request\njobs:\n  a: {runs-on: [self-hosted, windows]}\n", false},
		{"self-hosted, list", "on: [workflow_dispatch, pull_request_target]\njobs:\n  a: {runs-on: self-hosted}\n", false},
		{"self-hosted in a group's labels", "on: {workflow_run: {}}\njobs:\n  a: {runs-on: {group: g, labels: [self-hosted]}}\n", false},
		{"registered labels without self-hosted", "on: pull_request\njobs:\n  a: {runs-on: [windows, x64, isshoni-audio]}\n", false},
		{"one registered label", "on: pull_request\njobs:\n  a: {runs-on: win11}\n", false},
		{"registered label in another case", "on: pull_request\njobs:\n  a: {runs-on: [Win10]}\n", false},
		{"registered label in a mapping's labels", "on: pull_request\njobs:\n  a: {runs-on: {labels: [isshoni-audio]}}\n", false},
		{"runner group", "on: {pull_request: {}}\njobs:\n  a: {runs-on: {group: audio-pool}}\n", false},
		{"runner group with hosted labels", "on: {pull_request: {}}\njobs:\n  a: {runs-on: {group: g, labels: [ubuntu-24.04]}}\n", false},
		{"input runs-on", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ inputs.runner }}\"}\n", false},
		{"hosted matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [ubuntu-24.04, macos-15]}}}\n", true},
		{"self-hosted matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [ubuntu-24.04, self-hosted]}}}\n", false},
		{"registered label matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.os }}\", strategy: {matrix: {os: [ubuntu-24.04, win10]}}}\n", false},
		{"self-hosted matrix include", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.r }}\", strategy: {matrix: {include: [{r: ubuntu-24.04}, {r: [self-hosted, x64]}]}}}\n", false},
		{"registered label matrix include", "on: {pull_request: {}}\njobs:\n  a: {runs-on: [\"${{ matrix.r }}\"], strategy: {matrix: {include: [{r: ubuntu-24.04}, {r: isshoni-audio}]}}}\n", false},
		{"runner group matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.r }}\", strategy: {matrix: {include: [{r: {group: g}}]}}}\n", false},
		{"hosted labels mapping matrix", "on: {pull_request: {}}\njobs:\n  a: {runs-on: \"${{ matrix.r }}\", strategy: {matrix: {include: [{r: {labels: [ubuntu-24.04]}}]}}}\n", true},
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
			err = checkNoPRToSelfHosted(w, runnerLabels(audioRunnerLabels))
			if tc.ok && err != nil {
				t.Errorf("want no error, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}
