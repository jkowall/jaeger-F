// Copyright (c) 2026 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package fossacredential

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

// These tests follow the license-scan upload path and fail while a repository
// reader can authenticate it. They pass once that credential is no longer in
// the tree.
//
// The path, in order:
//   - actions/runner v2.338.2 TemplateReader.ParseScalar keeps a scalar that
//     does not contain "${{" as a literal (src/Sdk/WorkflowParser/ObjectTemplating/TemplateReader.cs).
//   - Handler.AddInputsToEnvironment copies that literal to INPUT_<NAME>
//     (src/Runner.Worker/Handlers/Handler.cs).
//   - @actions/core 3.0.1 getInput reads INPUT_<NAME> and trims it.
//   - fossa-contrib/fossa-action d28f0c947e7406706981a62a91ecc009bcc39fd7 puts
//     that value in the FOSSA_API_KEY environment and execs "fossa analyze"
//     (@actions/exec 3.0.0 passes that environment to the child).
//   - fossa-cli v3.20.13 validateApiKey takes the config apiKey, else that
//     environment variable, and uploadAnalysis POSTs it as
//     "Authorization: Bearer <key>" to https://app.fossa.com/api/builds/custom
//     with the locator rendered from .fossa.yml project.id.
//
// fossa test is not a second upload here: the action's skip-test input
// defaults to true and this workflow does not set it, so only analyze runs.
// The same environment variable would be passed to fossa test if skip-test
// were turned off.

const (
	fossaWorkflow   = ".github/workflows/fossa.yml"
	stage3Workflow  = ".github/workflows/ci-orchestrator-stage3.yml"
	orchWorkflow    = ".github/workflows/ci-orchestrator.yml"
	fossaConfigFile = ".fossa.yml"
	fossaAction     = "fossa-contrib/fossa-action@"
	uploadHost      = "https://app.fossa.com"
	uploadPath      = "/api/builds/custom"
	openExpression  = "${{"
)

func TestRepositoryReaderCanUploadFossaReport(t *testing.T) {
	root := moduleRoot(t)
	req := uploadRequestFromRepository(t, root)
	for _, path := range workflowFiles(t, root) {
		if path == filepath.Join(root, fossaWorkflow) {
			continue
		}
		text := readFile(t, path)
		if !strings.Contains(text, fossaAction) {
			continue
		}
		extra := uploadRequest(t, text, readFile(t, filepath.Join(root, fossaConfigFile)), "attacker-supplied-revision")
		if extra != nil {
			t.Fatalf("another workflow also authenticates a FOSSA upload: %s", path)
		}
	}
	if req == nil {
		return
	}

	var got *http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		got.Body = http.NoBody
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	sent := req.Clone(t.Context())
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	sent.URL.Scheme = serverURL.Scheme
	sent.URL.Host = serverURL.Host
	resp, err := http.DefaultClient.Do(sent)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.NotNil(t, got)
	require.Equal(t, http.MethodPost, got.Method)
	require.Equal(t, uploadPath, got.URL.Path)
	require.Equal(t, req.URL.Query().Get("locator"), got.URL.Query().Get("locator"))
	require.NotEmpty(t, got.Header.Get("Authorization"))

	t.Fatalf("a reader of %s can authenticate POST %s%s locator %s for the project in %s",
		fossaWorkflow, uploadHost, uploadPath, req.URL.Query().Get("locator"), fossaConfigFile)
}

func TestPullRequestOrchestratorUsesThatUpload(t *testing.T) {
	root := moduleRoot(t)
	orch := loadWorkflow(t, filepath.Join(root, orchWorkflow))
	require.True(t, mappingHasKey(t, triggerNode(t, orch), "pull_request"),
		"CI orchestrator must declare the pull_request trigger that runs for fork pull requests")

	for _, jobName := range []string{"stage3-seq", "stage3-fast"} {
		job := mappingChild(t, mappingChild(t, orch, "jobs"), jobName)
		require.Equal(t, "./"+stage3Workflow, scalar(t, mappingChild(t, job, "uses")))
	}

	stage3 := loadWorkflow(t, filepath.Join(root, stage3Workflow))
	fossaJob := mappingChild(t, mappingChild(t, stage3, "jobs"), "fossa")
	require.Equal(t, "./"+fossaWorkflow, scalar(t, mappingChild(t, fossaJob, "uses")))

	req := uploadRequestFromRepository(t, root)
	if req == nil {
		return
	}
	t.Fatalf("pull_request runs %s via stage 3, and that workflow still authenticates POST %s%s locator %s",
		fossaWorkflow, uploadHost, uploadPath, req.URL.Query().Get("locator"))
}

func TestLiteralInputIsTheUploadCredential(t *testing.T) {
	const projectID = "git+github.com/example/project"
	literal := workflowWithKey("placeholder-fossa-push-token")
	req := uploadRequest(t, literal, fossaConfig(projectID, ""), "attacker-supplied-revision")
	require.NotNil(t, req)
	require.Equal(t, uploadHost, req.URL.Scheme+"://"+req.URL.Host)
	require.Equal(t, uploadPath, req.URL.Path)
	require.Equal(t, "custom+"+projectID+"$attacker-supplied-revision", req.URL.Query().Get("locator"))
	require.Equal(t, "Bearer placeholder-fossa-push-token", req.Header.Get("Authorization"))
}

func TestSecretExpressionIsNotARepositoryCredential(t *testing.T) {
	workflow := workflowWithKey("${{ secrets.FOSSA_API_KEY }}")
	req := uploadRequest(t, workflow, fossaConfig("git+github.com/example/project", ""), "rev")
	require.Nil(t, req)
}

func TestRealWorkflowIsFixedByASecretExpression(t *testing.T) {
	root := moduleRoot(t)
	workflowText := readFile(t, filepath.Join(root, fossaWorkflow))
	keyLine := regexp.MustCompile(`(?m)^(\s*fossa-api-key:\s*).*$`)
	if !keyLine.MatchString(workflowText) {
		t.Fatal("fossa-api-key input not found")
	}
	fixed := keyLine.ReplaceAllString(workflowText, `${1}'${{ secrets.FOSSA_API_KEY }}'`)
	fixed = stripStandaloneHex(fixed)
	req := uploadRequest(t, fixed, readFile(t, filepath.Join(root, fossaConfigFile)), "attacker-supplied-revision")
	if req != nil {
		t.Fatal("replacing the fossa-api-key input with a secret expression still authenticates an upload")
	}
}

func TestConfigAPIKeyOverridesTheWorkflowInput(t *testing.T) {
	workflow := workflowWithKey("${{ secrets.FOSSA_API_KEY }}")
	cfg := fossaConfig("git+github.com/example/project", "placeholder-config-token")
	req := uploadRequest(t, workflow, cfg, "rev")
	require.NotNil(t, req, "an apiKey committed in .fossa.yml is the credential fossa-cli sends")
	require.Equal(t, "Bearer placeholder-config-token", req.Header.Get("Authorization"))
}

func uploadRequestFromRepository(t *testing.T, root string) *http.Request {
	t.Helper()
	configText := readFile(t, filepath.Join(root, fossaConfigFile))
	// Read the workflow at the moment the upload is built. A fix has to change
	// this file, which is the file the action reads, not an earlier copy.
	workflowText := readFile(t, filepath.Join(root, fossaWorkflow))
	return uploadRequest(t, workflowText, configText, "attacker-supplied-revision")
}

func TestCommittedTokenIsRemovedFromWorkflowFiles(t *testing.T) {
	root := moduleRoot(t)
	var files []string
	for _, rel := range []string{fossaConfigFile} {
		files = append(files, rel)
	}
	for _, path := range workflowFiles(t, root) {
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		files = append(files, rel)
	}
	for _, rel := range files {
		text := readFile(t, filepath.Join(root, rel))
		if hasStandaloneHexToken(text) {
			t.Errorf("remove the committed token from %s, including comments", rel)
		}
	}
}

// hasStandaloneHexToken reports whether text contains a 32-hex token that is
// not a prefix of a longer hex string such as an action pin.
func hasStandaloneHexToken(text string) bool {
	return stripStandaloneHex(text) != text
}

func stripStandaloneHex(text string) string {
	const hex = "0123456789abcdefABCDEF"
	var b strings.Builder
	for i := 0; i < len(text); {
		if i+32 <= len(text) && (i == 0 || !strings.ContainsRune(hex, rune(text[i-1]))) &&
			(i+32 == len(text) || !strings.ContainsRune(hex, rune(text[i+32]))) &&
			isHex(text[i:i+32]) {
			i += 32
			continue
		}
		b.WriteByte(text[i])
		i++
	}
	return b.String()
}

func isHex(s string) bool {
	if len(s) == 0 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func uploadRequest(t *testing.T, workflowText, configText, revision string) *http.Request {
	t.Helper()
	workflow := parseYAML(t, workflowText)
	config := parseYAML(t, configText)

	input := fossaAPIKeyInput(t, workflow)
	envKey := ""
	if input != "" && !strings.Contains(input, openExpression) {
		// TemplateReader.ParseScalar returns the scalar unchanged when it
		// does not contain "${{". AddInputsToEnvironment stores it, and
		// getInput trims it into the value the action exports.
		envKey = strings.TrimSpace(input)
	}
	configKey := configAPIKey(t, config)
	// fossa-cli validateApiKey: command-line option, then config apiKey, then
	// FOSSA_API_KEY. The action does not pass --fossa-api-key.
	key := configKey
	if key == "" {
		key = envKey
	}
	if key == "" {
		return nil
	}

	projectID := scalar(t, mappingChild(t, mappingChild(t, config, "project"), "id"))
	require.NotEmpty(t, projectID)
	// fossa-cli renderLocator: "<fetcher>+<project>$<revision>". uploadAnalysis
	// always uses the "custom" fetcher and the project name from .fossa.yml id.
	locator := "custom+" + projectID + "$" + revision

	endpoint := fossaEndpointInput(t, workflow)
	if endpoint == "" {
		endpoint = configServer(t, config)
	}
	if endpoint == "" {
		endpoint = uploadHost
	}
	endpoint = strings.TrimRight(endpoint, "/")

	form := url.Values{}
	form.Set("locator", locator)
	form.Set("managedBuild", "true")
	target, err := url.Parse(endpoint + uploadPath + "?" + form.Encode())
	require.NoError(t, err)

	body := strings.NewReader(`[{"source":"attacker-supplied-scan"}]`)
	req, err := http.NewRequest(http.MethodPost, target.String(), body)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	return req
}

func fossaAPIKeyInput(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	steps := fossaActionSteps(t, doc)
	require.NotEmpty(t, steps, "workflow must invoke %s", fossaAction)
	var found string
	var seen bool
	for _, step := range steps {
		with := mappingLookup(step, "with")
		require.NotNil(t, with, "fossa-action step is missing its with block")
		value := mappingLookup(with, "fossa-api-key")
		require.NotNil(t, value, "fossa-action step is missing fossa-api-key")
		require.Equal(t, yaml.ScalarNode, value.Kind)
		got := value.Value
		if seen {
			require.Equal(t, found, got, "every fossa-action step must use the same credential input")
		}
		found = got
		seen = true
		skipTest := mappingLookup(with, "skip-test")
		if skipTest != nil {
			require.Equal(t, "true", strings.TrimSpace(skipTest.Value),
				"skip-test false would also run fossa test with this credential")
		}
	}
	return found
}

func fossaEndpointInput(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	for _, step := range fossaActionSteps(t, doc) {
		with := mappingLookup(step, "with")
		if with == nil {
			continue
		}
		if endpoint := mappingLookup(with, "endpoint"); endpoint != nil {
			return strings.TrimSpace(endpoint.Value)
		}
	}
	return ""
}

func fossaActionSteps(t *testing.T, doc *yaml.Node) []*yaml.Node {
	t.Helper()
	jobs := mappingLookup(doc, "jobs")
	require.NotNil(t, jobs)
	var steps []*yaml.Node
	for _, job := range mappingValues(t, jobs) {
		seq := mappingLookup(job, "steps")
		if seq == nil {
			continue
		}
		require.Equal(t, yaml.SequenceNode, seq.Kind)
		for _, step := range seq.Content {
			uses := mappingLookup(step, "uses")
			if uses != nil && strings.Contains(uses.Value, fossaAction) {
				steps = append(steps, step)
			}
		}
	}
	return steps
}

func configAPIKey(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	node := mappingLookup(doc, "apiKey")
	if node == nil {
		return ""
	}
	require.Equal(t, yaml.ScalarNode, node.Kind)
	return strings.TrimSpace(node.Value)
}

func configServer(t *testing.T, doc *yaml.Node) string {
	t.Helper()
	node := mappingLookup(doc, "server")
	if node == nil {
		return ""
	}
	return strings.TrimSpace(node.Value)
}

func workflowWithKey(key string) string {
	return "jobs:\n  scan:\n    steps:\n      - uses: fossa-contrib/fossa-action@pinned\n        with:\n          fossa-api-key: " + quoteYAML(key) + "\n"
}

func fossaConfig(projectID, apiKey string) string {
	text := "version: 3\nproject:\n  id: " + quoteYAML(projectID) + "\n"
	if apiKey != "" {
		text += "apiKey: " + quoteYAML(apiKey) + "\n"
	}
	return text
}

func quoteYAML(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func loadWorkflow(t *testing.T, path string) *yaml.Node {
	t.Helper()
	return parseYAML(t, readFile(t, path))
}

func parseYAML(t *testing.T, text string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	require.NoError(t, yaml.Unmarshal([]byte(text), &doc))
	require.Equal(t, yaml.DocumentNode, doc.Kind)
	require.Len(t, doc.Content, 1)
	root := doc.Content[0]
	require.Equal(t, yaml.MappingNode, root.Kind)
	return root
}

func triggerNode(t *testing.T, doc *yaml.Node) *yaml.Node {
	t.Helper()
	if node := mappingLookup(doc, "on"); node != nil {
		return node
	}
	// A YAML 1.1 parser would treat the bare key "on" as boolean true.
	if node := mappingLookup(doc, "true"); node != nil {
		return node
	}
	t.Fatal("workflow is missing its trigger block")
	return nil
}

func mappingChild(t *testing.T, node *yaml.Node, key string) *yaml.Node {
	t.Helper()
	child := mappingLookup(node, key)
	require.NotNil(t, child, "missing %q", key)
	return child
}

func mappingHasKey(t *testing.T, node *yaml.Node, key string) bool {
	t.Helper()
	return mappingLookup(node, key) != nil
}

func mappingLookup(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func mappingValues(t *testing.T, node *yaml.Node) []*yaml.Node {
	t.Helper()
	require.Equal(t, yaml.MappingNode, node.Kind)
	var values []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		values = append(values, node.Content[i+1])
	}
	return values
}

func scalar(t *testing.T, node *yaml.Node) string {
	t.Helper()
	if node == nil {
		return ""
	}
	require.Equal(t, yaml.ScalarNode, node.Kind)
	return node.Value
}

func workflowFiles(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*"))
	require.NoError(t, err)
	return matches
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "go.mod not found")
		dir = parent
	}
}
