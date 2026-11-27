// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package codecovtoken

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// These tests follow the coverage upload path that starts at the committed
// composite action and ends at Codecov project and patch statuses.
//
// The steps below are the transformations that code actually performs. Each
// one is cited in the function that implements it. The tests fail while a
// literal upload token in this repository is what that path sends as the
// upload credential, and they pass once that credential is no longer a
// literal committed in the tree.

const (
	attackerCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	attackerSlug   = "jaegertracing/jaeger"
	// Obvious placeholder. This is not a credential from the repository.
	placeholderToken = "00000000-0000-4000-8000-000000000000"
	ingestURL        = "https://ingest.codecov.io"
	// UploadsPerCommitThrottle allows another upload while the existing
	// session count is not greater than the owner default of 150.
	defaultMaxUploadsPerCommit = 150
)

func TestCommittedCodecovTokenAuthorizesForgedStatuses(t *testing.T) {
	root := moduleRoot(t)
	// Read the action at use time. A check that ran earlier and cached a
	// different answer must not be what this assertion uses.
	outcome := forgeCoverageStatuses(t, root, attackerCommit, 100)

	if outcome.Authorized && outcome.ProjectPass && outcome.PatchPass {
		t.Fatalf("literal Codecov upload token in %s authorizes a crafted coverage upload for commit %s and posts success for %s and %s (uploads=%d, after_n_builds=%d)",
			outcome.TokenFile, attackerCommit, outcome.ProjectContext, outcome.PatchContext, outcome.Uploads, outcome.AfterNBuilds)
	}
}

func TestSecretExpressionDoesNotAuthorizeUpload(t *testing.T) {
	action := actionYAML("token: ${{ secrets.CODECOV_TOKEN }}")
	outcome := forgeCoverageStatusesFromAction(t, action, codecovYAML(27, 95, 95), attackerCommit, 100)
	if outcome.Authorized {
		t.Fatal("a secrets expression was treated as a committed upload credential")
	}
}

func TestEmptyTokenDoesNotAuthorizeUpload(t *testing.T) {
	action := actionYAML("")
	outcome := forgeCoverageStatusesFromAction(t, action, codecovYAML(27, 95, 95), attackerCommit, 100)
	if outcome.Authorized {
		t.Fatal("an absent upload token authorized a coverage upload")
	}
}

func TestPlaceholderUploadTokenProducesPassingStatuses(t *testing.T) {
	action := actionYAML("token: " + placeholderToken)
	outcome := forgeCoverageStatusesFromAction(t, action, codecovYAML(27, 95, 95), attackerCommit, 100)
	if !outcome.Authorized || !outcome.ProjectPass || !outcome.PatchPass {
		t.Fatalf("placeholder upload token did not produce passing statuses: authorized=%v project=%v patch=%v",
			outcome.Authorized, outcome.ProjectPass, outcome.PatchPass)
	}
	if outcome.CommitSHA != attackerCommit {
		t.Fatalf("upload commit = %s, want attacker commit", outcome.CommitSHA)
	}
	if outcome.ProjectContext != "codecov/project" || outcome.PatchContext != "codecov/patch" {
		t.Fatalf("status contexts = %s %s", outcome.ProjectContext, outcome.PatchContext)
	}
}

func TestNotificationsWaitUntilConfiguredUploadCount(t *testing.T) {
	action := actionYAML("token: " + placeholderToken)
	cfgText := codecovYAML(27, 95, 95)
	cfg := parseCodecovConfig(t, cfgText)
	short := forgeCoverageStatusesFromAction(t, action, cfgText, attackerCommit, 100)
	// Re-run the notifier with one fewer session than the configured threshold.
	if notify(cfg, 26, 100, emptyCIStatuses()).ProjectPass {
		t.Fatal("notify ran before after_n_builds uploads existed")
	}
	if !short.ProjectPass || short.Uploads != 27 {
		t.Fatalf("attacker able to meet after_n_builds: pass=%v uploads=%d", short.ProjectPass, short.Uploads)
	}
}

func TestCoverageBelowTargetFailsStatuses(t *testing.T) {
	action := actionYAML("token: " + placeholderToken)
	outcome := forgeCoverageStatusesFromAction(t, action, codecovYAML(1, 95, 95), attackerCommit, 10)
	if !outcome.Authorized {
		t.Fatal("placeholder token should still authenticate")
	}
	if outcome.ProjectPass || outcome.PatchPass {
		t.Fatal("coverage below the configured target posted a passing status")
	}
}

func TestEmptyCIStatusSetDoesNotBlockRequireCIToPass(t *testing.T) {
	// GitHub Actions contexts are not in the worker CI allow-list, so the
	// filtered status set is empty. An empty set is success, and
	// require_ci_to_pass only suppresses notification when CI failed.
	if emptyCIStatuses() != trueResult {
		t.Fatal("empty filtered CI status set was not treated as success")
	}
	if !shouldSendNotifications(true, trueResult, 27, 27) {
		t.Fatal("require_ci_to_pass blocked notification when CI had not failed")
	}
	if shouldSendNotifications(true, falseResult, 27, 27) {
		t.Fatal("require_ci_to_pass sent notifications after CI failed")
	}
}

type ciResult int

const (
	unknownResult ciResult = iota
	trueResult
	falseResult
)

type codecovConfig struct {
	AfterNBuilds  int
	ProjectOn     bool
	PatchOn       bool
	ProjectTarget float64
	PatchTarget   float64
	RequireCI     bool
}

type outcome struct {
	Authorized     bool
	CommitSHA      string
	ProjectPass    bool
	PatchPass      bool
	ProjectContext string
	PatchContext   string
	Uploads        int
	AfterNBuilds   int
	TokenFile      string
}

func forgeCoverageStatuses(t *testing.T, root, commit string, coverage float64) outcome {
	t.Helper()
	actionPath := filepath.Join(root, ".github", "actions", "upload-codecov", "action.yml")
	action, err := os.ReadFile(actionPath)
	if err != nil {
		t.Fatalf("read upload action: %v", err)
	}
	cfgText, err := os.ReadFile(filepath.Join(root, ".codecov.yml"))
	if err != nil {
		t.Fatalf("read codecov config: %v", err)
	}
	result := forgeCoverageStatusesFromAction(t, string(action), string(cfgText), commit, coverage)
	result.TokenFile = ".github/actions/upload-codecov/action.yml"
	if other := literalTokenOutsideAction(root, actionPath); other != "" {
		t.Fatalf("literal Codecov upload token also committed in %s", other)
	}
	return result
}

func forgeCoverageStatusesFromAction(t *testing.T, actionYAML, codecovYAML, commit string, coverage float64) outcome {
	t.Helper()
	cfg := parseCodecovConfig(t, codecovYAML)
	token, useOIDC := codecovTokenInput(actionYAML)
	// The attacker does not run the workflow. They read the literal that the
	// workflow would pass to codecov-action and call the upload API with it.
	credential := committedCredential(token, useOIDC)
	request := buildUploadRequest(credential, attackerSlug, commit)
	auth := legacyTokenAuth(request.authorization, credential)
	out := outcome{AfterNBuilds: cfg.AfterNBuilds, CommitSHA: request.commitSHA}
	if !auth.allowsUpload || auth.repositoryToken != credential || credential == "" {
		return out
	}
	if request.commitSHA != commit {
		t.Fatalf("upload URL commit %s is not the attacker commit %s", request.commitSHA, commit)
	}
	// create-commit stores the client-supplied commitid. The token is not
	// compared to that commit.
	created := createCommit(auth, request.commitSHA)
	if created != request.commitSHA {
		t.Fatal("created commit is not the commit the attacker named")
	}
	uploads := uploadsAttackerCanSend(cfg.AfterNBuilds)
	out.Authorized = true
	out.Uploads = uploads
	note := notify(cfg, uploads, coverage, emptyCIStatuses())
	out.ProjectPass = note.ProjectPass
	out.PatchPass = note.PatchPass
	out.ProjectContext = note.ProjectContext
	out.PatchContext = note.PatchContext
	return out
}

// committedCredential is the value an attacker can copy out of the repository.
// A GitHub Actions expression is not that value: the runner substitutes it
// from a secret that forks cannot read. use_oidc changes what the workflow
// itself sends, and the literal left in the file is still what an attacker
// copies. OIDC therefore does not clear a committed token.
func committedCredential(token string, _ bool) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, `"'`)
	if token == "" || isExpression(token) {
		return ""
	}
	return token
}

func isExpression(value string) bool {
	return strings.HasPrefix(value, "${{") && strings.HasSuffix(value, "}}")
}

type uploadRequest struct {
	authorization string
	commitSHA     string
}

// buildUploadRequest is codecov-action buildUploadExec followed by the CLI
// do-upload request. getInput reads INPUT_TOKEN, and a non-empty token that
// is not replaced by OIDC is copied to CODECOV_TOKEN. The CLI --token option
// reads CODECOV_TOKEN and get_token_header sends it as
// "Authorization: token <value>". -C / override_commit is placed in the
// upload URL path unchanged.
func buildUploadRequest(codecovToken, slug, commit string) uploadRequest {
	inputs := map[string]string{
		"token":            codecovToken,
		"files":            "cover.out",
		"flags":            "forged",
		"verbose":          "true",
		"fail_ci_if_error": "false",
		"override_commit":  commit,
		"git_service":      "github",
	}
	env := envOptionsFrom(inputs)
	token := getInput(env, "token")
	useOIDC := isTrue(getInput(env, "use_oidc"))
	if useOIDC {
		token = ""
	}
	var authorization string
	if token != "" {
		authorization = "token " + token
	}
	service := getInput(env, "git_service")
	if service == "" {
		service = "github"
	}
	if !validUploadService(service) {
		return uploadRequest{}
	}
	sha := getInput(env, "override_commit")
	if sha == "" {
		sha = "workflow-sha-not-used"
	}
	_ = uploadURL(service, encodeSlug(slug), sha)
	return uploadRequest{authorization: authorization, commitSHA: sha}
}

// envOptionsFrom is wretry.action Common.envOptionsFrom: each parsed `with`
// key becomes INPUT_<KEY>.
func envOptionsFrom(options map[string]string) map[string]string {
	env := make(map[string]string, len(options))
	for key, value := range options {
		name := "INPUT_" + strings.ToUpper(strings.ReplaceAll(key, " ", "_"))
		env[name] = value
	}
	return env
}

// getInput is @actions/core getInput from codecov-action's bundled dist:
// process.env["INPUT_" + name] trimmed.
func getInput(env map[string]string, name string) string {
	return strings.TrimSpace(env["INPUT_"+strings.ToUpper(strings.ReplaceAll(name, " ", "_"))])
}

func isTrue(value string) bool {
	switch strings.ToLower(value) {
	case "1", "t", "true", "y", "yes":
		return true
	default:
		return false
	}
}

func validUploadService(service string) bool {
	switch service {
	case "github", "gitlab", "bitbucket", "github_enterprise", "gitlab_enterprise", "bitbucket_server", "github-actions":
		return true
	default:
		return false
	}
}

func encodeSlug(slug string) string {
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return ""
	}
	return owner + "::::" + repo
}

func uploadURL(service, encodedSlug, commit string) string {
	return ingestURL + "/upload/" + service + "/" + encodedSlug + "/commits/" + commit + "/reports/default/uploads"
}

type legacyAuth struct {
	allowsUpload    bool
	repositoryToken string
}

// legacyTokenAuth is DRF TokenAuthentication.authenticate plus
// RepositoryLegacyTokenAuthentication.authenticate_credentials.
// The header keyword is compared case-insensitively to "Token". The
// credential must be one UUID, and it authorizes only the repository whose
// upload_token equals that UUID. The commit SHA is not an input to auth.
func legacyTokenAuth(header, repositoryUploadToken string) legacyAuth {
	if repositoryUploadToken == "" || !isUUID(repositoryUploadToken) {
		return legacyAuth{}
	}
	parts := strings.Split(header, " ")
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Token") {
		return legacyAuth{}
	}
	presented := parts[1]
	if !isUUID(presented) {
		return legacyAuth{}
	}
	if !strings.EqualFold(presented, repositoryUploadToken) {
		return legacyAuth{}
	}
	return legacyAuth{allowsUpload: true, repositoryToken: repositoryUploadToken}
}

func createCommit(auth legacyAuth, commitSHA string) string {
	if !auth.allowsUpload {
		return ""
	}
	// CommitSerializer.create persists the client-supplied commitid.
	return commitSHA
}

func uploadsAttackerCanSend(afterN int) int {
	needed := afterN
	if needed <= 0 {
		needed = 1
	}
	if needed > defaultMaxUploadsPerCommit {
		return defaultMaxUploadsPerCommit
	}
	return needed
}

type notification struct {
	ProjectPass    bool
	PatchPass      bool
	ProjectContext string
	PatchContext   string
}

func notify(cfg codecovConfig, sessions int, coverage float64, ci ciResult) notification {
	if !shouldSendNotifications(cfg.RequireCI, ci, cfg.AfterNBuilds, sessions) {
		return notification{}
	}
	project, patch := "codecov/project", "codecov/patch"
	return notification{
		ProjectPass:    cfg.ProjectOn && coverage >= cfg.ProjectTarget,
		PatchPass:      cfg.PatchOn && coverage >= cfg.PatchTarget,
		ProjectContext: project,
		PatchContext:   patch,
	}
}

// shouldSendNotifications is NotifyTask.should_send_notifications.
// require_ci_to_pass blocks only when ci_passed is False. after_n_builds
// blocks while the report has fewer sessions than that number.
func shouldSendNotifications(requireCI bool, ci ciResult, afterN, sessions int) bool {
	if requireCI && ci == falseResult {
		return false
	}
	if afterN > 0 && afterN > sessions {
		return false
	}
	return true
}

// ciPassed is NotifyTask.fetch_and_update_whether_ci_passed.
// Status.__init__ sets state to success when every remaining state is
// success. all([]) is success, so an empty filtered set is success.
func ciPassed(statuses []string) ciResult {
	state := statusState(statuses)
	switch state {
	case "success":
		return trueResult
	case "failure":
		return falseResult
	default:
		return unknownResult
	}
}

func statusState(states []string) string {
	if len(states) == 0 || allSuccess(states) {
		return "success"
	}
	for _, state := range states {
		if state == "failure" || state == "error" {
			return "failure"
		}
	}
	for _, state := range states {
		if state == "pending" {
			return "pending"
		}
	}
	return "failure"
}

func allSuccess(states []string) bool {
	for _, state := range states {
		if state != "success" {
			return false
		}
	}
	return true
}

// emptyCIStatuses is the set left after RepositoryCIFilter drops GitHub
// Actions statuses. Their target URL host is github.com, which is not in
// CI_DOMAINS, and their context is not in CI_CONTEXTS.
func emptyCIStatuses() ciResult {
	return ciPassed(nil)
}

var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func isUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

func codecovTokenInput(actionYAML string) (token string, useOIDC bool) {
	lines := strings.Split(actionYAML, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "codecov/codecov-action@") && !strings.Contains(line, "codecov/codecov-action/") {
			continue
		}
		token, useOIDC = tokenFromFollowingWith(lines[i+1:])
		if token != "" || useOIDC {
			return token, useOIDC
		}
	}
	return "", false
}

func tokenFromFollowingWith(lines []string) (string, bool) {
	for i, line := range lines {
		indent, payload, ok := splitIndent(line)
		if !ok {
			continue
		}
		if strings.HasPrefix(payload, "uses:") || strings.HasPrefix(payload, "- ") {
			return "", false
		}
		if payload != "with:" && payload != "with: |" && payload != "with: |-" {
			continue
		}
		fields := map[string]string{}
		for _, next := range lines[i+1:] {
			nextIndent, nextPayload, ok := splitIndent(next)
			if !ok {
				continue
			}
			if len(nextIndent) <= len(indent) {
				break
			}
			key, value, ok := strings.Cut(nextPayload, ":")
			if !ok {
				continue
			}
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
		return fields["token"], isTrue(fields["use_oidc"])
	}
	return "", false
}

func splitIndent(line string) (indent, payload string, ok bool) {
	if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
		return "", "", false
	}
	trimmed := strings.TrimLeft(line, " ")
	return line[:len(line)-len(trimmed)], strings.TrimSpace(trimmed), true
}

func literalTokenOutsideAction(root, actionPath string) string {
	var found string
	_ = filepath.WalkDir(filepath.Join(root, ".github"), func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || found != "" {
			return nil
		}
		if path == actionPath {
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}
		text, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		token, _ := codecovTokenInput(string(text))
		if committedCredential(token, false) != "" {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			found = rel
		}
		return nil
	})
	return found
}

func parseCodecovConfig(t *testing.T, text string) codecovConfig {
	t.Helper()
	cfg := codecovConfig{ProjectOn: true, PatchOn: true, RequireCI: true, ProjectTarget: 95, PatchTarget: 95}
	lines := strings.Split(text, "\n")
	section := ""
	statusKind := ""
	for _, line := range lines {
		_, payload, ok := splitIndent(line)
		if !ok {
			continue
		}
		key, value, hasValue := strings.Cut(payload, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "codecov", "coverage", "status", "notify":
			if value == "" {
				section = key
			}
		case "project", "patch":
			if section == "status" || strings.Contains(text, "status:") {
				statusKind = key
			}
		}
		if key == "require_ci_to_pass" && hasValue {
			cfg.RequireCI = isTrue(value) || value == "yes"
		}
		if key == "after_n_builds" && hasValue {
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("after_n_builds %q: %v", value, err)
			}
			cfg.AfterNBuilds = n
		}
		if key == "enabled" && hasValue && (statusKind == "project" || statusKind == "patch") {
			on := isTrue(value) || value == "yes"
			if statusKind == "project" {
				cfg.ProjectOn = on
			} else {
				cfg.PatchOn = on
			}
		}
		if key == "target" && hasValue && (statusKind == "project" || statusKind == "patch") {
			number := strings.TrimSuffix(value, "%")
			parsed, err := strconv.ParseFloat(number, 64)
			if err != nil {
				t.Fatalf("target %q: %v", value, err)
			}
			if statusKind == "project" {
				cfg.ProjectTarget = parsed
			} else {
				cfg.PatchTarget = parsed
			}
		}
	}
	return cfg
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func actionYAML(tokenLine string) string {
	body := ""
	if tokenLine != "" {
		body = "\n          " + tokenLine
	}
	return "name: Upload coverage to codecov\nruns:\n  using: composite\n  steps:\n    - uses: Wandalen/wretry.action@71a909ebf09f3ffdc6f42a17bd54ecb43481da49\n      with:\n        action: codecov/codecov-action@7afa10ed9b269c561c2336fd862446844e0cbf71\n        with: |" + body + "\n          files: cover.out\n"
}

func codecovYAML(afterN int, project, patch float64) string {
	return strings.Join([]string{
		"codecov:",
		"  notify:",
		"    require_ci_to_pass: yes",
		"    after_n_builds: " + strconv.Itoa(afterN),
		"coverage:",
		"  status:",
		"    project:",
		"      default:",
		"        enabled: yes",
		"        target: " + strconv.FormatFloat(project, 'f', -1, 64) + "%",
		"    patch:",
		"      default:",
		"        enabled: yes",
		"        target: " + strconv.FormatFloat(patch, 'f', -1, 64) + "%",
	}, "\n")
}
