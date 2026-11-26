// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package ghaexec

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/jaegertracing/jaeger/internal/testutils"
)

func TestMain(m *testing.M) {
	testutils.VerifyGoLeaks(m)
}

// These tests are the spec for the publish-release credential exposure.
// They fail while a workflow_dispatch (or a same-repo pull_request merge
// commit) can read long-lived credentials from a ref other than the
// protected default branch. They are expected to fail until that is fixed.
//
// The interpreter follows the rules read for this commit:
//   - The workflow file that runs is the file at the event SHA
//     (docs: "Workflows", "Each workflow run will use the version of the
//     workflow that is present in the associated commit SHA").
//   - workflow_dispatch sets GITHUB_SHA to the tip of the selected ref
//     (docs: "Events that trigger workflows", workflow_dispatch table).
//   - Repository secrets are passed to that run. They are withheld for a
//     forked pull_request, and they are not returned by
//     GET /repos/{owner}/{repo}/actions/secrets/{secret_name}
//     (docs: "Using secrets in GitHub Actions"; REST "Get a repository secret").
//   - A job or step if has an implicit success() check unless it already
//     calls success, failure, always, or cancelled
//     (docs: "Evaluate expressions in workflows and actions").
//   - actions/checkout v7.0.0 (9c091bb) getInputs falls back to github.ref
//     and github.sha when ref is empty, and treats a 40/64 hex ref as a
//     commit (input-helper.ts:61-77).
//   - The runner passes the step environment, including secrets interpolated
//     into env, to the process (actions/runner v2.338.2 ScriptHandler.cs:339-343).
//   - ghaction-import-gpg 2dc316d writes the signing key into the GnuPG
//     keyring and deletes it only in the post step (gpg.ts:131-137,
//     main.ts:151-154), so later steps in the job can export it.
//   - Environment secrets are available only to jobs that name the
//     environment, and only after protection rules pass
//     (docs: "Managing environments for deployment"; workflow syntax
//     jobs.<job_id>.environment). This file treats a credential as an
//     environment secret only when every committed reference sits in a job
//     that sets environment. Untrusted refs still do not receive it, and a
//     reviewer must approve before a trusted ref does.

const protectedRef = "refs/heads/main"

var (
	secretRefPattern = regexp.MustCompile(`secrets\.([A-Za-z0-9_]+)`)
	semverTagPattern = regexp.MustCompile(`^refs/tags/v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$`)
	shaPattern       = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

func TestDispatchOffDefaultBranchDoesNotRunCredentialSteps(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-release.yml")
	for _, dryRun := range []bool{false, true} {
		t.Run(fmt.Sprintf("dry_run=%t", dryRun), func(t *testing.T) {
			uses := executedCredentialUses(t, workflow, event{
				name:       "workflow_dispatch",
				repository: "jaegertracing/jaeger",
				ref:        "refs/heads/attacker",
				sha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				inputs: map[string]any{
					"dry_run":   dryRun,
					"overwrite": false,
					"ref":       "",
				},
			})
			if len(uses) > 0 {
				t.Fatalf("workflow_dispatch on refs/heads/attacker still materializes credentials:\n%s", formatUses(uses))
			}
		})
	}
}

func TestDispatchOnMainDoesNotCheckOutUntrustedInputsRef(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-release.yml")
	const (
		mainSHA     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		attackerSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	uses := executedCredentialUses(t, workflow, event{
		name:       "workflow_dispatch",
		repository: "jaegertracing/jaeger",
		ref:        protectedRef,
		sha:        mainSHA,
		inputs: map[string]any{
			"dry_run":   false,
			"overwrite": false,
			"ref":       attackerSHA,
		},
	})
	var leaked []credentialUse
	for _, use := range uses {
		if use.checkoutUntrusted {
			leaked = append(leaked, use)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("dispatch on main with inputs.ref %s still gives credentials to that checkout:\n%s", attackerSHA, formatUses(leaked))
	}
}

func TestReleaseOnSemverTagStillRunsCredentialSteps(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-release.yml")
	uses := executedCredentialUses(t, workflow, event{
		name:       "release",
		repository: "jaegertracing/jaeger",
		ref:        "refs/tags/v2.999.0",
		sha:        "cccccccccccccccccccccccccccccccccccccccc",
	})
	if len(uses) == 0 {
		t.Fatal("a published semver release no longer reaches signing or registry credentials")
	}
}

func TestWrongRepositoryDoesNotRunPublishJob(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-release.yml")
	uses := executedCredentialUses(t, workflow, event{
		name:       "workflow_dispatch",
		repository: "example/fork",
		ref:        protectedRef,
		sha:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		inputs: map[string]any{
			"dry_run":   false,
			"overwrite": false,
			"ref":       "",
		},
	})
	require.Empty(t, uses)
}

func TestGuardedRecoveryWorkflowsDoNotRunOffMain(t *testing.T) {
	root := repoRoot(t)
	for _, path := range []string{
		".github/workflows/ci-opensearch-recovery.yml",
		".github/workflows/ci-opensearch-volume-expand.yml",
	} {
		t.Run(path, func(t *testing.T) {
			workflow := loadWorkflow(t, root, path)
			uses := executedCredentialUses(t, workflow, event{
				name:       "workflow_dispatch",
				repository: "jaegertracing/jaeger",
				ref:        "refs/heads/attacker",
				sha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			})
			require.Empty(t, uses)
		})
	}
}

func TestDeployDemoDispatchOffMainDoesNotRunCredentialSteps(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-deploy-demo.yml")
	uses := executedCredentialUses(t, workflow, event{
		name:       "workflow_dispatch",
		repository: "jaegertracing/jaeger",
		ref:        "refs/heads/attacker",
		sha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if len(uses) > 0 {
		t.Fatalf("demo deploy dispatch on refs/heads/attacker still materializes credentials:\n%s", formatUses(uses))
	}
}

func TestSubstitutedWorkflowOnUntrustedRefGetsNoCredentials(t *testing.T) {
	root := repoRoot(t)
	cases := []event{
		{
			name:       "workflow_dispatch",
			repository: "jaegertracing/jaeger",
			ref:        "refs/heads/attacker",
			sha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name:           "workflow_dispatch",
			repository:     "jaegertracing/jaeger",
			ref:            "refs/heads/attacker",
			sha:            "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			reviewApproved: true,
		},
		{
			name:       "release",
			repository: "jaegertracing/jaeger",
			ref:        "refs/tags/v2.999.0",
			sha:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			name:       "pull_request",
			repository: "jaegertracing/jaeger",
			ref:        "refs/pull/1/merge",
			sha:        "dddddddddddddddddddddddddddddddddddddddd",
		},
	}
	for _, ev := range cases {
		t.Run(fmt.Sprintf("%s/%s/approved=%t", ev.name, ev.ref, ev.reviewApproved), func(t *testing.T) {
			got := credentialsInjectedIntoSubstitutedWorkflow(t, root, ev)
			if len(got) > 0 {
				t.Fatalf("a workflow file from %s %s still receives %s", ev.name, ev.ref, strings.Join(got, ", "))
			}
		})
	}
}

func TestForkPullRequestDoesNotReceiveCredentials(t *testing.T) {
	root := repoRoot(t)
	got := credentialsInjectedIntoSubstitutedWorkflow(t, root, event{
		name:       "pull_request",
		repository: "jaegertracing/jaeger",
		ref:        "refs/pull/1/merge",
		sha:        "dddddddddddddddddddddddddddddddddddddddd",
		fork:       true,
	})
	require.Empty(t, got)
}

func TestManualDispatchFromMainStillReachesCredentials(t *testing.T) {
	root := repoRoot(t)
	workflow := loadWorkflow(t, root, ".github/workflows/ci-release.yml")
	uses := executedCredentialUses(t, workflow, event{
		name:           "workflow_dispatch",
		repository:     "jaegertracing/jaeger",
		ref:            protectedRef,
		sha:            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		reviewApproved: true,
		inputs: map[string]any{
			"dry_run":   false,
			"overwrite": false,
			"ref":       "",
		},
	})
	if len(uses) == 0 {
		t.Fatal("manual dispatch from main with an empty inputs.ref no longer reaches credentials")
	}
	for _, use := range uses {
		if use.checkoutUntrusted {
			t.Fatalf("manual dispatch from main checked out an untrusted ref while using %s in %s", use.secret, use.where)
		}
	}
}

type event struct {
	name           string
	repository     string
	ref            string
	sha            string
	fork           bool
	reviewApproved bool
	cancelled      bool
	inputs         map[string]any
}

type credentialUse struct {
	job               string
	where             string
	secret            string
	checkoutUntrusted bool
	checkoutRef       string
	checkoutCommit    string
}

func (u credentialUse) String() string {
	checkout := u.checkoutCommit
	if checkout == "" {
		checkout = u.checkoutRef
	}
	if checkout == "" {
		checkout = "(no checkout)"
	}
	return fmt.Sprintf("%s %s secret %s checkout %s", u.job, u.where, u.secret, checkout)
}

func formatUses(uses []credentialUse) string {
	parts := make([]string, len(uses))
	for i, use := range uses {
		parts[i] = use.String()
	}
	return strings.Join(parts, "\n")
}

type loadedWorkflow struct {
	path string
	doc  map[string]any
}

func executedCredentialUses(t *testing.T, workflow loadedWorkflow, ev event) []credentialUse {
	t.Helper()
	jobs, _ := asMap(workflow.doc["jobs"])
	order := jobOrder(jobs)
	status := map[string]string{}
	outputs := map[string]map[string]val{}
	var uses []credentialUse
	for _, name := range order {
		job, _ := asMap(jobs[name])
		ran, jobUses, outs := runJob(t, name, job, ev, status, outputs)
		if ran {
			status[name] = "success"
		} else {
			status[name] = "skipped"
		}
		outputs[name] = outs
		uses = append(uses, jobUses...)
	}
	return uses
}

func runJob(t *testing.T, name string, job map[string]any, ev event, status map[string]string, prior map[string]map[string]val) (bool, []credentialUse, map[string]val) {
	t.Helper()
	needs := stringList(job["needs"])
	needsOK := true
	for _, need := range needs {
		if status[need] != "success" {
			needsOK = false
		}
	}
	condText, hasIf := scalarString(job["if"])
	run := evalJobCondition(t, condText, hasIf, ev, needsOK, prior)
	if !run {
		return false, nil, nil
	}
	checkoutRef, checkoutCommit := resolveJobCheckout(t, job, ev)
	untrusted := checkoutUntrusted(checkoutRef, checkoutCommit, ev)
	ctx := newContext(ev, prior, nil)
	var uses []credentialUse
	if envMap, ok := asMap(job["env"]); ok {
		for key, raw := range envMap {
			text, ok := raw.(string)
			if !ok {
				continue
			}
			value := evalInterpolated(t, text, ctx)
			if value.rawSecret != "" {
				uses = append(uses, credentialUse{
					job: name, where: "job env " + key, secret: value.rawSecret,
					checkoutUntrusted: untrusted, checkoutRef: checkoutRef, checkoutCommit: checkoutCommit,
				})
			}
			ctx.env[key] = stringify(value)
		}
	}
	steps, _ := asList(job["steps"])
	stepOut := map[string]map[string]val{}
	// An untrusted checkout can make an earlier step fail on purpose, so a
	// later if: failure() step still runs with the job's credentials.
	forceFailure := untrusted || attackerControlsWorkspace(ev)
	for i, rawStep := range steps {
		step, ok := asMap(rawStep)
		if !ok {
			continue
		}
		ctx.steps = stepOut
		stepIf, hasStepIf := scalarString(step["if"])
		successPath := evalStepCondition(t, stepIf, hasStepIf, ev, true, prior, ctx)
		failurePath := forceFailure && !successPath && evalStepCondition(t, stepIf, hasStepIf, ev, false, prior, ctx)
		if !successPath && !failurePath {
			continue
		}
		title := stepTitle(step, i)
		site := useSite{job: name, where: title, untrusted: untrusted, checkoutRef: checkoutRef, checkoutCommit: checkoutCommit}
		recordSecrets(t, &uses, site, step["env"], ctx)
		recordSecrets(t, &uses, site, step["with"], ctx)
		if !successPath {
			continue
		}
		if id, ok := step["id"].(string); ok && id == "branch" {
			stepOut[id] = map[string]val{"tag": {k: vkStr, s: "v2.999.0"}}
		}
		if id, ok := step["id"].(string); ok && id == "hash" {
			stepOut[id] = map[string]val{"hashes": {k: vkStr, s: "aGFzaA"}}
		}
	}
	outs := map[string]val{}
	ctx.steps = stepOut
	if outMap, ok := asMap(job["outputs"]); ok {
		for key, raw := range outMap {
			text, ok := raw.(string)
			if !ok {
				continue
			}
			outs[key] = evalInterpolated(t, text, ctx)
		}
	}
	return true, uses, outs
}

type useSite struct {
	job            string
	where          string
	untrusted      bool
	checkoutRef    string
	checkoutCommit string
}

func recordSecrets(t *testing.T, uses *[]credentialUse, site useSite, node any, ctx *exprContext) {
	t.Helper()
	env, ok := asMap(node)
	if !ok {
		return
	}
	for key, raw := range env {
		text, ok := raw.(string)
		if !ok {
			continue
		}
		value := evalInterpolated(t, text, ctx)
		if value.rawSecret == "" || ignoredSecret(value.rawSecret) {
			continue
		}
		*uses = append(*uses, credentialUse{
			job: site.job, where: site.where + " " + key, secret: value.rawSecret,
			checkoutUntrusted: site.untrusted, checkoutRef: site.checkoutRef, checkoutCommit: site.checkoutCommit,
		})
	}
}

func resolveJobCheckout(t *testing.T, job map[string]any, ev event) (string, string) {
	t.Helper()
	steps, _ := asList(job["steps"])
	ref, commit := "", ""
	seen := false
	for _, rawStep := range steps {
		step, ok := asMap(rawStep)
		if !ok {
			continue
		}
		uses, _ := step["uses"].(string)
		if !strings.Contains(uses, "actions/checkout") {
			continue
		}
		seen = true
		with, _ := asMap(step["with"])
		refInput := ""
		if raw, ok := with["ref"]; ok {
			text, _ := raw.(string)
			refInput = stringify(evalInterpolated(t, text, newContext(ev, nil, nil)))
		}
		// actions/checkout 9c091bb input-helper.ts:61-77.
		switch {
		case refInput == "":
			ref = ev.ref
			commit = ev.sha
			if commit != "" && ref != "" && !strings.HasPrefix(ref, "refs/") {
				ref = "refs/heads/" + ref
			}
		case shaPattern.MatchString(refInput):
			commit = refInput
			ref = ""
		default:
			ref = refInput
			commit = ""
		}
	}
	if !seen {
		return "", ""
	}
	return ref, commit
}

func attackerControlsWorkspace(ev event) bool {
	switch ev.name {
	case "workflow_dispatch":
		return ev.ref != protectedRef
	case "pull_request":
		return !ev.fork
	default:
		return false
	}
}

func checkoutUntrusted(ref, commit string, ev event) bool {
	if ref == "" && commit == "" {
		return false
	}
	if commit != "" && commit == ev.sha {
		return false
	}
	if commit == "" && ref != "" && ref == ev.ref {
		return false
	}
	return true
}

func evalJobCondition(t *testing.T, text string, hasIf bool, ev event, needsOK bool, prior map[string]map[string]val) bool {
	t.Helper()
	if !hasIf {
		return needsOK && !ev.cancelled
	}
	return evalCondition(t, text, ev, needsOK, prior, nil)
}

func evalStepCondition(t *testing.T, text string, hasIf bool, ev event, previousOK bool, prior map[string]map[string]val, ctx *exprContext) bool {
	t.Helper()
	if !hasIf {
		return previousOK
	}
	return evalCondition(t, text, ev, previousOK, prior, ctx)
}

func evalCondition(t *testing.T, text string, ev event, previousOK bool, prior map[string]map[string]val, ctx *exprContext) bool {
	t.Helper()
	inner := unwrapExpr(text)
	if !statusCallPattern.MatchString(inner) {
		inner = "success() && (" + inner + ")"
	}
	if ctx == nil {
		ctx = newContext(ev, prior, nil)
	} else {
		ctx.ev = ev
		ctx.needs = prior
		ctx.previousOK = previousOK
	}
	ctx.previousOK = previousOK
	value := parseAndEval(t, inner, ctx)
	return truthy(value)
}

var statusCallPattern = regexp.MustCompile(`\b(success|failure|always|cancelled)\s*\(`)

func stepTitle(step map[string]any, index int) string {
	if name, ok := step["name"].(string); ok && name != "" {
		return strconv.Quote(name)
	}
	if uses, ok := step["uses"].(string); ok && uses != "" {
		return uses
	}
	return fmt.Sprintf("step %d", index+1)
}

type secretBinding int

const (
	repositorySecret secretBinding = iota
	environmentSecret
)

func credentialsInjectedIntoSubstitutedWorkflow(t *testing.T, root string, ev event) []string {
	t.Helper()
	if ev.fork {
		// Docs: with the exception of GITHUB_TOKEN, secrets are not passed
		// when a workflow is triggered from a forked repository.
		return nil
	}
	refs := credentialReferences(t, root)
	byName := map[string][]credentialRef{}
	for _, ref := range refs {
		byName[ref.name] = append(byName[ref.name], ref)
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	var got []string
	for _, name := range names {
		binding := environmentSecret
		var envName string
		for _, ref := range byName[name] {
			if ref.environment == "" {
				binding = repositorySecret
				break
			}
			envName = ref.environment
		}
		if binding == repositorySecret {
			// The substituted file is the file at the event SHA. Repository
			// secrets are still injected. The attacker's job keeps the
			// repository-name check, which passes for jaegertracing/jaeger.
			if strings.EqualFold(ev.repository, "jaegertracing/jaeger") {
				got = append(got, name)
			}
			continue
		}
		if !ev.reviewApproved || !environmentRefAllowed(ev) {
			continue
		}
		if envName == "" || !strings.EqualFold(ev.repository, "jaegertracing/jaeger") {
			continue
		}
		got = append(got, name)
	}
	return got
}

func environmentRefAllowed(ev event) bool {
	switch ev.name {
	case "workflow_dispatch", "push", "schedule":
		return ev.ref == protectedRef
	case "release":
		return semverTagPattern.MatchString(ev.ref)
	default:
		return false
	}
}

type credentialRef struct {
	file        string
	job         string
	environment string
	name        string
}

func credentialReferences(t *testing.T, root string) []credentialRef {
	t.Helper()
	paths := workflowPaths(t, root)
	var refs []credentialRef
	for _, path := range paths {
		workflow := loadWorkflow(t, root, path)
		jobs, _ := asMap(workflow.doc["jobs"])
		for jobName, rawJob := range jobs {
			job, ok := asMap(rawJob)
			if !ok {
				continue
			}
			envName, _ := environmentOf(job)
			walkStrings(job, func(text string) {
				for _, match := range secretRefPattern.FindAllStringSubmatch(text, -1) {
					name := match[1]
					if ignoredSecret(name) {
						continue
					}
					refs = append(refs, credentialRef{
						file: path, job: jobName, environment: envName, name: name,
					})
				}
			})
		}
	}
	return refs
}

func ignoredSecret(name string) bool {
	return name == "GITHUB_TOKEN"
}

func environmentOf(job map[string]any) (string, bool) {
	switch env := job["environment"].(type) {
	case string:
		return env, env != ""
	case map[string]any:
		name, _ := env["name"].(string)
		return name, name != ""
	default:
		return "", false
	}
}

func walkStrings(node any, fn func(string)) {
	switch typed := node.(type) {
	case map[string]any:
		for _, value := range typed {
			walkStrings(value, fn)
		}
	case []any:
		for _, value := range typed {
			walkStrings(value, fn)
		}
	case string:
		fn(typed)
	}
}

func workflowPaths(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".github/workflows/*.yml"))
	require.NoError(t, err)
	more, err := filepath.Glob(filepath.Join(root, ".github/workflows/*.yaml"))
	require.NoError(t, err)
	matches = append(matches, more...)
	sort.Strings(matches)
	var rels []string
	for _, match := range matches {
		rel, err := filepath.Rel(root, match)
		require.NoError(t, err)
		rels = append(rels, rel)
	}
	return rels
}

func loadWorkflow(t *testing.T, root, rel string) loadedWorkflow {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	doc, err := parseWorkflow(data)
	require.NoError(t, err)
	return loadedWorkflow{path: rel, doc: doc}
}

func parseWorkflow(data []byte) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("workflow document is empty")
	}
	value := nodeValue(doc.Content[0])
	mapped, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("workflow root is not a mapping")
	}
	return mapped, nil
}

func nodeValue(node *yaml.Node) any {
	switch node.Kind {
	case yaml.MappingNode:
		mapped := make(map[string]any, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			mapped[node.Content[i].Value] = nodeValue(node.Content[i+1])
		}
		return mapped
	case yaml.SequenceNode:
		items := make([]any, len(node.Content))
		for i, child := range node.Content {
			items[i] = nodeValue(child)
		}
		return items
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!bool":
			return strings.EqualFold(node.Value, "true") || node.Value == "yes" || node.Value == "on"
		case "!!int":
			parsed, err := strconv.ParseInt(node.Value, 0, 64)
			if err != nil {
				return node.Value
			}
			return parsed
		case "!!float":
			parsed, err := strconv.ParseFloat(node.Value, 64)
			if err != nil {
				return node.Value
			}
			return parsed
		case "!!null":
			return nil
		default:
			return node.Value
		}
	case yaml.AliasNode:
		if node.Alias == nil {
			return nil
		}
		return nodeValue(node.Alias)
	default:
		return nil
	}
}

func jobOrder(jobs map[string]any) []string {
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	var ordered []string
	var visit func(string)
	visit = func(name string) {
		if seen[name] {
			return
		}
		seen[name] = true
		job, _ := asMap(jobs[name])
		for _, need := range stringList(job["needs"]) {
			if _, ok := jobs[need]; ok {
				visit(need)
			}
		}
		ordered = append(ordered, name)
	}
	for _, name := range names {
		visit(name)
	}
	return ordered
}

func stringList(node any) []string {
	switch typed := node.(type) {
	case string:
		if typed == "" {
			return nil
		}
		return []string{typed}
	case []any:
		var out []string
		for _, item := range typed {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func asMap(node any) (map[string]any, bool) {
	mapped, ok := node.(map[string]any)
	return mapped, ok
}

func asList(node any) ([]any, bool) {
	list, ok := node.([]any)
	return list, ok
}

func scalarString(node any) (string, bool) {
	switch typed := node.(type) {
	case string:
		return typed, true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir)
		dir = parent
	}
}

type valKind int

const (
	vkNull valKind = iota
	vkBool
	vkNum
	vkStr
)

type val struct {
	k         valKind
	b         bool
	n         float64
	nan       bool
	s         string
	rawSecret string
}

type exprContext struct {
	ev         event
	needs      map[string]map[string]val
	env        map[string]string
	steps      map[string]map[string]val
	previousOK bool
}

func newContext(ev event, needs map[string]map[string]val, steps map[string]map[string]val) *exprContext {
	return &exprContext{ev: ev, needs: needs, env: map[string]string{}, steps: steps, previousOK: true}
}

func evalInterpolated(t *testing.T, text string, ctx *exprContext) val {
	t.Helper()
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "${{") && strings.HasSuffix(trimmed, "}}") {
		return parseAndEval(t, unwrapExpr(trimmed), ctx)
	}
	return val{k: vkStr, s: text}
}

func unwrapExpr(text string) string {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "${{") && strings.HasSuffix(trimmed, "}}") {
		return strings.TrimSpace(trimmed[3 : len(trimmed)-2])
	}
	return trimmed
}

func parseAndEval(t *testing.T, text string, ctx *exprContext) val {
	t.Helper()
	parser := &parser{tokens: lex(text)}
	value := parser.parseOr()
	require.Falsef(t, parser.failed, "cannot parse expression %q: %s", text, parser.err)
	return evalNode(value, ctx)
}

type node interface{ eval(*exprContext) val }

type binaryNode struct {
	op    string
	left  node
	right node
}

func (n *binaryNode) eval(ctx *exprContext) val {
	switch n.op {
	case "||":
		if truthy(n.left.eval(ctx)) {
			return val{k: vkBool, b: true}
		}
		return val{k: vkBool, b: truthy(n.right.eval(ctx))}
	case "&&":
		if !truthy(n.left.eval(ctx)) {
			return val{k: vkBool, b: false}
		}
		return val{k: vkBool, b: truthy(n.right.eval(ctx))}
	default:
		return compare(n.op, n.left.eval(ctx), n.right.eval(ctx))
	}
}

type unaryNode struct {
	op   string
	expr node
}

func (n *unaryNode) eval(ctx *exprContext) val {
	value := n.expr.eval(ctx)
	if n.op == "!" {
		return val{k: vkBool, b: !truthy(value)}
	}
	return value
}

type pathNode struct {
	parts []string
}

func (n *pathNode) eval(ctx *exprContext) val {
	if len(n.parts) == 0 {
		return val{}
	}
	switch n.parts[0] {
	case "github":
		return lookup(n.parts[1:], map[string]val{
			"repository": {k: vkStr, s: ctx.ev.repository},
			"ref":        {k: vkStr, s: ctx.ev.ref},
			"sha":        {k: vkStr, s: ctx.ev.sha},
			"ref_name":   {k: vkStr, s: refName(ctx.ev.ref)},
			"event_name": {k: vkStr, s: ctx.ev.name},
		})
	case "inputs":
		if len(n.parts) != 2 {
			return val{}
		}
		raw, ok := ctx.ev.inputs[n.parts[1]]
		if !ok {
			return val{}
		}
		return fromGo(raw)
	case "secrets":
		if len(n.parts) != 2 || ignoredSecret(n.parts[1]) {
			return val{}
		}
		// Presence only. The value is never a real credential.
		return val{k: vkStr, s: "configured", rawSecret: n.parts[1]}
	case "env":
		if len(n.parts) != 2 {
			return val{}
		}
		text, ok := ctx.env[n.parts[1]]
		if !ok {
			return val{}
		}
		return val{k: vkStr, s: text}
	case "needs":
		return lookupNeeds(n.parts[1:], ctx.needs)
	case "steps":
		return lookupSteps(n.parts[1:], ctx.steps)
	default:
		return val{}
	}
}

type callNode struct {
	name string
	args []node
}

func (n *callNode) eval(ctx *exprContext) val {
	switch n.name {
	case "success":
		return val{k: vkBool, b: ctx.previousOK && !ctx.ev.cancelled}
	case "failure":
		return val{k: vkBool, b: !ctx.previousOK}
	case "always":
		return val{k: vkBool, b: true}
	case "cancelled":
		return val{k: vkBool, b: ctx.ev.cancelled}
	case "startsWith":
		if len(n.args) != 2 {
			return val{k: vkBool, b: false}
		}
		return val{k: vkBool, b: strings.HasPrefix(strings.ToLower(stringify(n.args[0].eval(ctx))), strings.ToLower(stringify(n.args[1].eval(ctx))))}
	case "endsWith":
		if len(n.args) != 2 {
			return val{k: vkBool, b: false}
		}
		return val{k: vkBool, b: strings.HasSuffix(strings.ToLower(stringify(n.args[0].eval(ctx))), strings.ToLower(stringify(n.args[1].eval(ctx))))}
	default:
		return val{}
	}
}

type literalNode struct{ value val }

func (n literalNode) eval(*exprContext) val { return n.value }

func evalNode(n node, ctx *exprContext) val { return n.eval(ctx) }

func lookup(parts []string, fields map[string]val) val {
	if len(parts) != 1 {
		return val{}
	}
	value, ok := fields[parts[0]]
	if !ok {
		return val{}
	}
	return value
}

func lookupNeeds(parts []string, needs map[string]map[string]val) val {
	// needs.<job>.outputs.<name>
	if len(parts) != 3 || parts[1] != "outputs" || needs == nil {
		return val{}
	}
	outs := needs[parts[0]]
	if outs == nil {
		return val{}
	}
	value, ok := outs[parts[2]]
	if !ok {
		return val{}
	}
	return value
}

func lookupSteps(parts []string, steps map[string]map[string]val) val {
	// steps.<id>.outputs.<name>
	if len(parts) != 3 || parts[1] != "outputs" || steps == nil {
		return val{}
	}
	outs := steps[parts[0]]
	if outs == nil {
		return val{}
	}
	value, ok := outs[parts[2]]
	if !ok {
		return val{}
	}
	return value
}

func fromGo(raw any) val {
	switch typed := raw.(type) {
	case nil:
		return val{}
	case bool:
		return val{k: vkBool, b: typed}
	case string:
		return val{k: vkStr, s: typed}
	case int:
		return val{k: vkNum, n: float64(typed)}
	case int64:
		return val{k: vkNum, n: float64(typed)}
	case float64:
		return val{k: vkNum, n: typed}
	default:
		return val{k: vkStr, s: fmt.Sprint(typed)}
	}
}

func truthy(v val) bool {
	switch v.k {
	case vkBool:
		return v.b
	case vkNum:
		return !v.nan && v.n != 0
	case vkStr:
		return v.s != ""
	default:
		return false
	}
}

func stringify(v val) string {
	switch v.k {
	case vkBool:
		return strconv.FormatBool(v.b)
	case vkNum:
		if v.nan {
			return "NaN"
		}
		return strconv.FormatFloat(v.n, 'f', -1, 64)
	case vkStr:
		return v.s
	default:
		return ""
	}
}

func refName(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

func compare(op string, left, right val) val {
	if left.k == vkStr && right.k == vkStr {
		equal := strings.EqualFold(left.s, right.s)
		return compareEqual(op, equal, left.s, right.s)
	}
	if left.k == right.k && !left.nan && !right.nan {
		switch left.k {
		case vkBool:
			return compareEqual(op, left.b == right.b, boolRank(left.b), boolRank(right.b))
		case vkNum:
			return compareEqual(op, left.n == right.n, left.n, right.n)
		case vkNull:
			return compareEqual(op, true, 0, 0)
		}
	}
	ln, lnan := asNumber(left)
	rn, rnan := asNumber(right)
	if lnan || rnan {
		switch op {
		case "==":
			return val{k: vkBool, b: false}
		case "!=":
			return val{k: vkBool, b: true}
		default:
			return val{k: vkBool, b: false}
		}
	}
	return compareEqual(op, ln == rn, ln, rn)
}

func boolRank(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func compareEqual(op string, equal bool, left, right any) val {
	switch op {
	case "==":
		return val{k: vkBool, b: equal}
	case "!=":
		return val{k: vkBool, b: !equal}
	}
	lf, lok := numeric(left)
	rf, rok := numeric(right)
	if !lok || !rok {
		return val{k: vkBool, b: false}
	}
	switch op {
	case "<":
		return val{k: vkBool, b: lf < rf}
	case "<=":
		return val{k: vkBool, b: lf <= rf}
	case ">":
		return val{k: vkBool, b: lf > rf}
	case ">=":
		return val{k: vkBool, b: lf >= rf}
	default:
		return val{k: vkBool, b: false}
	}
}

func numeric(v any) (float64, bool) {
	switch typed := v.(type) {
	case float64:
		return typed, true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}

func asNumber(v val) (float64, bool) {
	switch v.k {
	case vkNull:
		return 0, false
	case vkBool:
		return boolRank(v.b), false
	case vkNum:
		return v.n, v.nan
	case vkStr:
		if v.s == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(v.s, 64)
		if err != nil {
			return 0, true
		}
		return parsed, false
	default:
		return 0, true
	}
}

type parser struct {
	tokens []token
	pos    int
	failed bool
	err    string
}

func (p *parser) parseOr() node {
	left := p.parseAnd()
	for p.peek().val == "||" {
		p.next()
		right := p.parseAnd()
		left = &binaryNode{op: "||", left: left, right: right}
	}
	return left
}

func (p *parser) parseAnd() node {
	left := p.parseNot()
	for p.peek().val == "&&" {
		p.next()
		right := p.parseNot()
		left = &binaryNode{op: "&&", left: left, right: right}
	}
	return left
}

func (p *parser) parseNot() node {
	if p.peek().val == "!" {
		p.next()
		return &unaryNode{op: "!", expr: p.parseNot()}
	}
	return p.parseCmp()
}

func (p *parser) parseCmp() node {
	left := p.parsePrimary()
	op := p.peek().val
	switch op {
	case "==", "!=", "<", "<=", ">", ">=":
		p.next()
		right := p.parsePrimary()
		return &binaryNode{op: op, left: left, right: right}
	default:
		return left
	}
}

func (p *parser) parsePrimary() node {
	tok := p.peek()
	switch tok.kind {
	case "num":
		p.next()
		parsed, err := strconv.ParseFloat(tok.val, 64)
		if err != nil {
			p.fail(err.Error())
			return literalNode{val{k: vkNum, nan: true}}
		}
		return literalNode{val{k: vkNum, n: parsed}}
	case "str":
		p.next()
		return literalNode{val{k: vkStr, s: tok.val}}
	case "ident":
		if tok.val == "true" || tok.val == "false" {
			p.next()
			return literalNode{val{k: vkBool, b: tok.val == "true"}}
		}
		if tok.val == "null" {
			p.next()
			return literalNode{val{}}
		}
		p.next()
		parts := []string{tok.val}
		for p.peek().val == "." {
			p.next()
			part := p.next()
			if part.kind != "ident" {
				p.fail("expected property name")
				break
			}
			parts = append(parts, part.val)
		}
		if p.peek().val == "(" {
			p.next()
			var args []node
			if p.peek().val != ")" {
				for {
					args = append(args, p.parseOr())
					if p.peek().val != "," {
						break
					}
					p.next()
				}
			}
			if p.peek().val != ")" {
				p.fail("expected )")
			} else {
				p.next()
			}
			return &callNode{name: parts[0], args: args}
		}
		return &pathNode{parts: parts}
	case "op":
		if tok.val == "(" {
			p.next()
			expr := p.parseOr()
			if p.peek().val != ")" {
				p.fail("expected )")
			} else {
				p.next()
			}
			return expr
		}
	}
	p.fail("unexpected token " + tok.val)
	p.next()
	return literalNode{val{}}
}

func (p *parser) peek() token {
	if p.pos >= len(p.tokens) {
		return token{kind: "eof"}
	}
	return p.tokens[p.pos]
}

func (p *parser) next() token {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func (p *parser) fail(msg string) {
	if !p.failed {
		p.failed = true
		p.err = msg
	}
}

type token struct {
	kind string
	val  string
}

func lex(text string) []token {
	var tokens []token
	i := 0
	for i < len(text) {
		if unicode.IsSpace(rune(text[i])) {
			i++
			continue
		}
		rest := text[i:]
		switch {
		case strings.HasPrefix(rest, "||"), strings.HasPrefix(rest, "&&"), strings.HasPrefix(rest, "=="), strings.HasPrefix(rest, "!="), strings.HasPrefix(rest, "<="), strings.HasPrefix(rest, ">="):
			tokens = append(tokens, token{kind: "op", val: rest[:2]})
			i += 2
		case strings.ContainsRune("!<>().", rune(text[i])):
			tokens = append(tokens, token{kind: "op", val: rest[:1]})
			i++
		case text[i] == '\'':
			j := i + 1
			var b strings.Builder
			for j < len(text) {
				if text[j] == '\'' {
					if j+1 < len(text) && text[j+1] == '\'' {
						b.WriteByte('\'')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(text[j])
				j++
			}
			if j < len(text) && text[j] == '\'' {
				j++
			}
			tokens = append(tokens, token{kind: "str", val: b.String()})
			i = j
		default:
			if isIdentStart(text[i]) {
				j := i + 1
				for j < len(text) && isIdentPart(text[j]) {
					j++
				}
				tokens = append(tokens, token{kind: "ident", val: text[i:j]})
				i = j
				continue
			}
			if text[i] >= '0' && text[i] <= '9' {
				j := i + 1
				for j < len(text) && ((text[j] >= '0' && text[j] <= '9') || text[j] == '.') {
					j++
				}
				tokens = append(tokens, token{kind: "num", val: text[i:j]})
				i = j
				continue
			}
			tokens = append(tokens, token{kind: "op", val: rest[:1]})
			i++
		}
	}
	return tokens
}

func isIdentStart(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || b == '_'
}

func isIdentPart(b byte) bool {
	return isIdentStart(b) || (b >= '0' && b <= '9') || b == '-'
}
