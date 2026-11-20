# Copyright (c) 2026 The Jaeger Authors.
# SPDX-License-Identifier: Apache-2.0

"""Security spec for demo deploys that can be started from a non-default ref.

These tests fail while a workflow_dispatch of a ref other than the protected
default branch can configure the demo cluster or run that ref's deploy script
with the cloud credential environment. They pass once that dispatch is refused
before any of those steps, the checkout is the protected branch itself, and
scripts/demo/deploy.py rejects every other ref before it creates a file or
runs a child process.
"""

from pathlib import Path
import os
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from types import SimpleNamespace
from unittest.mock import patch

import yaml

DEMO_DIR = Path(__file__).resolve().parent
if str(DEMO_DIR) not in sys.path:
    sys.path.insert(0, str(DEMO_DIR))

import deploy  # noqa: E402

ROOT = deploy.ROOT
PROTECTED_REF = "refs/heads/main"
UNTRUSTED_REF = "refs/heads/untrusted-demo"
ATTACKER_SHA = "a" * 40
TRUSTED_SHA = "b" * 40
PLACEHOLDER_KEY = "placeholder-oci-key"
PLACEHOLDER_CLUSTER = "placeholder-cluster-ocid"
CLOUD_SECRET_NAMES = (
    "OCI_CLI_USER",
    "OCI_CLI_TENANCY",
    "OCI_CLI_FINGERPRINT",
    "OCI_CLI_KEY_CONTENT",
    "OCI_CLI_REGION",
    "OKE_CLUSTER_OCID",
)
SECRETS = {
    "OCI_CLI_USER": "placeholder-oci-user",
    "OCI_CLI_TENANCY": "placeholder-oci-tenancy",
    "OCI_CLI_FINGERPRINT": "placeholder-oci-fingerprint",
    "OCI_CLI_KEY_CONTENT": PLACEHOLDER_KEY,
    "OCI_CLI_REGION": "placeholder-oci-region",
    "OKE_CLUSTER_OCID": PLACEHOLDER_CLUSTER,
    "SLACK_WEBHOOK_URL": "placeholder-slack-webhook",
}
SHA_INPUT = re.compile(r"^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$")
STATUS_FUNCTION = re.compile(r"\b(?:success|failure|always|cancelled)\s*\(")
EXPR = re.compile(r"\$\{\{\s*(.*?)\s*\}\}", re.S)
REPO_SCRIPT = re.compile(r"(scripts/demo/deploy\.py|examples/\S+)")

ATTACKER_DEPLOY = textwrap.dedent(
    """\
    import os
    from pathlib import Path
    marker = Path(os.environ["ATTACK_MARKER"])
    watched = (
        "OCI_CLI_KEY_CONTENT",
        "OCI_CLI_USER",
        "OCI_CLI_TENANCY",
        "OCI_CLI_FINGERPRINT",
        "OCI_CLI_REGION",
        "OKE_CLUSTER_OCID",
    )
    lines = []
    for name in watched:
        lines.append(name + ("=set" if os.environ.get(name) else "=missing"))
    marker.write_text("\\n".join(lines) + "\\n")
    """
)


def load_workflow(path):
    document = yaml.safe_load(path.read_text())
    # PyYAML 1.1 treats the mapping key "on" as boolean true.
    if True in document and "on" not in document:
        document["on"] = document.pop(True)
    return document


def cloud_workflows():
    workflows = []
    for path in sorted((ROOT / ".github/workflows").glob("*.yml")):
        text = path.read_text()
        if "secrets.OCI_CLI_KEY_CONTENT" in text or "secrets.OKE_CLUSTER_OCID" in text:
            workflows.append(path)
    return workflows


def split_top(expr, operator):
    parts = []
    buf = []
    depth = 0
    quote = ""
    index = 0
    while index < len(expr):
        char = expr[index]
        if quote:
            buf.append(char)
            if char == quote:
                quote = ""
            index += 1
            continue
        if char in "'\"":
            quote = char
            buf.append(char)
            index += 1
            continue
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
        if depth == 0 and expr.startswith(operator, index):
            parts.append("".join(buf))
            buf = []
            index += len(operator)
            continue
        buf.append(char)
        index += 1
    parts.append("".join(buf))
    return parts


def wrapped(expr):
    if not (expr.startswith("(") and expr.endswith(")")):
        return False
    depth = 0
    for index, char in enumerate(expr):
        if char == "(":
            depth += 1
        elif char == ")":
            depth -= 1
            if depth == 0:
                return index == len(expr) - 1
    return False


def lookup(root, path):
    current = root
    for part in path.split("."):
        if isinstance(current, dict) and part in current:
            current = current[part]
        else:
            return ""
    return "" if current is None else current


def eval_value(expr, github):
    expr = expr.strip()
    parts = split_top(expr, "||")
    if len(parts) > 1:
        for part in parts:
            value = eval_value(part, github)
            if value:
                return value
        return ""
    if len(expr) >= 2 and expr[0] == expr[-1] and expr[0] in "'\"":
        return expr[1:-1]
    if expr.startswith("secrets."):
        return SECRETS.get(expr[len("secrets."):], "")
    if expr.startswith("github."):
        return lookup(github, expr[len("github."):])
    return expr


def interpolate(value, github):
    if not isinstance(value, str) or "${{" not in value:
        return "" if value is None else value

    def replace(match):
        return str(eval_value(match.group(1), github))

    return EXPR.sub(replace, value)


def normalize_if(raw):
    """Match WorkflowTemplateConverter.ConvertToIfCondition in actions/runner v2.338.2.

    An omitted condition becomes success(). A condition that does not already
    call a status function becomes success() && (<condition>).
    """
    if raw is None:
        return "success()"
    text = str(raw).strip()
    match = re.fullmatch(r"\$\{\{\s*(.*?)\s*\}\}", text, re.S)
    if match:
        text = match.group(1).strip()
    if not text:
        return "success()"
    if STATUS_FUNCTION.search(text):
        return text
    return f"success() && ({text})"


def eval_condition(expr, state, github):
    expr = expr.strip()
    while wrapped(expr):
        expr = expr[1:-1].strip()
    parts = split_top(expr, "&&")
    if len(parts) > 1:
        return all(eval_condition(part, state, github) for part in parts)
    parts = split_top(expr, "||")
    if len(parts) > 1:
        return any(eval_condition(part, state, github) for part in parts)
    if re.fullmatch(r"success\s*\(\s*\)", expr):
        return state["success"]
    if re.fullmatch(r"failure\s*\(\s*\)", expr):
        return state["failure"]
    if re.fullmatch(r"always\s*\(\s*\)", expr):
        return True
    if re.fullmatch(r"cancelled\s*\(\s*\)", expr):
        return False
    match = re.fullmatch(r"(.+?)\s*(==|!=)\s*(.+)", expr)
    if not match:
        raise AssertionError(f"unsupported workflow condition: {expr}")
    left = eval_value(match.group(1), github)
    right = eval_value(match.group(3), github)
    if match.group(2) == "==":
        return left == right
    return left != right


def needs_of(job):
    needs = job.get("needs") or []
    if isinstance(needs, str):
        return [needs]
    return list(needs)


def allows_workflow_dispatch(document):
    trigger = document.get("on")
    if trigger == "workflow_dispatch":
        return True
    if isinstance(trigger, list):
        return "workflow_dispatch" in trigger
    if isinstance(trigger, dict):
        return "workflow_dispatch" in trigger
    return False


def resolve_checkout(ref_input, github_ref, github_sha):
    """actions/checkout v7 getInputs at 9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0.

    An empty ref input checks out github.context.ref and github.context.sha.
    A 40- or 64-character ref input is the commit, with the ref cleared.
    """
    ref = (ref_input or "").strip()
    commit = None
    if not ref:
        ref = github_ref or ""
        commit = github_sha or ""
        if commit and ref and not ref.startswith("refs/"):
            ref = f"refs/heads/{ref}"
    elif SHA_INPUT.fullmatch(ref):
        commit = ref
        ref = ""
    return ref, commit or None


def workspace_for(ref, commit, trusted, attacker, empty):
    # getRefSpec pins refs/heads/* to settings.commit when that commit is set,
    # so the tree is the commit even when the ref name is the protected branch.
    if commit == ATTACKER_SHA or ref == UNTRUSTED_REF:
        return attacker
    if ref == PROTECTED_REF and commit in (None, TRUSTED_SHA):
        return trusted
    return empty


def github_context(ref, sha):
    return {
        "ref": ref,
        "sha": sha,
        "event_name": "workflow_dispatch",
        "event": {"inputs": {}},
        "run_id": "1000",
        "run_attempt": "1",
        "repository": "example/jaeger",
        "workflow": "demo",
        "actor": "example-actor",
        "server_url": "https://github.example",
    }


def continue_on_error(step):
    value = step.get("continue-on-error", False)
    return value is True or str(value).strip().lower() == "true"


class Trace:
    def __init__(self, marker):
        self.events = []
        self.checkouts = []
        self.marker = marker

    def problems(self):
        found = []
        for event in self.events:
            if event.startswith("cloud:") or event.startswith("repository-script:"):
                found.append(event)
        for ref, commit in self.checkouts:
            if commit == ATTACKER_SHA or ref == UNTRUSTED_REF:
                found.append(f"checkout ref={ref or '<empty>'} commit={commit}")
        if self.marker.exists():
            found.append("branch deploy.py ran:\n" + self.marker.read_text())
        return found


def run_dispatch(workflow_path, ref, sha, root):
    root.mkdir(parents=True, exist_ok=True)
    document = load_workflow(workflow_path)
    marker = root / "attack-marker"
    trace = Trace(marker)
    if not allows_workflow_dispatch(document):
        trace.events.append("dispatch-not-configured")
        return trace

    github = github_context(ref, sha)
    attacker = root / "attacker"
    deploy_py = attacker / "scripts" / "demo" / "deploy.py"
    deploy_py.parent.mkdir(parents=True)
    deploy_py.write_text(ATTACKER_DEPLOY)
    empty = root / "empty"
    empty.mkdir()
    trusted = ROOT
    stubs = root / "stubs"
    stubs.mkdir()
    stub_body = textwrap.dedent(
        """\
        #!/usr/bin/env python3
        import os
        from pathlib import Path
        name = Path(__file__).name
        key = "set" if os.environ.get("OCI_CLI_KEY_CONTENT") else "missing"
        with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as handle:
            handle.write(f"{name} key={key}\\n")
        """
    )
    for name in ("kubectl", "bash", "helm"):
        path = stubs / name
        path.write_text(stub_body)
        path.chmod(0o755)
    stub_log = root / "stub-log"
    workspace = empty
    real_bash = shutil.which("bash")
    if not real_bash:
        raise AssertionError("bash is required to execute workflow run steps")

    pending = dict(document.get("jobs") or {})
    results = {}
    while pending:
        ready = [name for name, job in pending.items() if all(item in results for item in needs_of(job))]
        if not ready:
            raise AssertionError(f"unresolved job needs in {workflow_path.name}: {sorted(pending)}")
        for name in ready:
            job = pending.pop(name)
            need_results = {item: results[item] for item in needs_of(job)}
            failed_need = any(result == "failure" for result in need_results.values())
            succeeded = all(result == "success" for result in need_results.values())
            job_state = {"success": succeeded and not failed_need, "failure": failed_need}
            if not eval_condition(normalize_if(job.get("if")), job_state, github):
                results[name] = "skipped"
                trace.events.append(f"skip-job:{name}")
                continue
            job_env = {}
            for key, value in (job.get("env") or {}).items():
                job_env[str(key)] = str(interpolate(value, github))
            step_state = {"success": True, "failure": False}
            job_failed = False
            for step in job.get("steps") or []:
                if not eval_condition(normalize_if(step.get("if")), step_state, github):
                    trace.events.append(f"skip-step:{step.get('name', step.get('uses', 'run'))}")
                    continue
                step_env = dict(job_env)
                for key, value in (step.get("env") or {}).items():
                    step_env[str(key)] = str(interpolate(value, github))
                step_env["GITHUB_REF"] = ref
                step_env["GITHUB_SHA"] = sha
                step_env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
                label = step.get("name") or step.get("uses") or "run"
                trace.events.append(f"step:{label}")
                uses = step.get("uses") or ""
                failed = False
                if uses.startswith("actions/checkout@"):
                    ref_input = interpolate((step.get("with") or {}).get("ref", ""), github)
                    checked_ref, checked_commit = resolve_checkout(ref_input, ref, sha)
                    trace.checkouts.append((checked_ref, checked_commit))
                    trace.events.append(
                        f"checkout ref={checked_ref or '<empty>'} commit={checked_commit or '<branch>'}"
                    )
                    workspace = workspace_for(checked_ref, checked_commit, trusted, attacker, empty)
                elif "configure-kubectl-oke" in uses:
                    cluster = interpolate((step.get("with") or {}).get("cluster", ""), github)
                    key_state = "set" if step_env.get("OCI_CLI_KEY_CONTENT") else "missing"
                    cluster_state = "set" if cluster else "missing"
                    trace.events.append(
                        f"cloud:configure-kubectl-oke key={key_state} cluster={cluster_state}"
                    )
                elif step.get("run") is not None:
                    script = str(step["run"])
                    repo_script = REPO_SCRIPT.search(script)
                    if repo_script:
                        trace.events.append(f"repository-script:{script.strip().splitlines()[0]}")
                    process_env = os.environ.copy()
                    for secret_name in CLOUD_SECRET_NAMES:
                        process_env.pop(secret_name, None)
                    process_env.pop("OKE_CLUSTER_OCID", None)
                    process_env.update(step_env)
                    process_env["PATH"] = str(stubs) + os.pathsep + process_env.get("PATH", "")
                    process_env["ATTACK_MARKER"] = str(marker)
                    process_env["STUB_LOG"] = str(stub_log)
                    script_path = root / "step.sh"
                    script_path.write_text(script)
                    completed = subprocess.run(
                        [real_bash, "-e", str(script_path)],
                        cwd=workspace,
                        env=process_env,
                        text=True,
                        capture_output=True,
                        timeout=60,
                        check=False,
                    )
                    failed = completed.returncode != 0
                if failed and not continue_on_error(step):
                    job_failed = True
                    step_state = {"success": False, "failure": True}
            results[name] = "failure" if job_failed else "success"
    return trace


class RefChangesAfterFirstRead(dict):
    """First GITHUB_REF read is the untrusted ref. Later reads report main.

    A check that reads the ref once and refuses still passes. A check that
    reads it again and trusts the later value does not.
    """

    def __init__(self, values):
        super().__init__(values)
        self.reads = 0

    def _ref(self):
        self.reads += 1
        if self.reads == 1:
            return UNTRUSTED_REF
        return PROTECTED_REF

    def get(self, key, default=None):
        if key == "GITHUB_REF":
            return self._ref()
        return super().get(key, default)

    def __getitem__(self, key):
        if key == "GITHUB_REF":
            return self._ref()
        return super().__getitem__(key)


class UntrustedDemoDispatchTests(unittest.TestCase):
    def test_expression_defaults_expose_cloud_credentials(self):
        document = load_workflow(ROOT / ".github/workflows/ci-deploy-demo.yml")
        github = github_context(UNTRUSTED_REF, ATTACKER_SHA)
        env = {key: interpolate(value, github) for key, value in document["jobs"]["deploy"]["env"].items()}
        self.assertEqual("required", env["JAEGER_OTEL_DEMO_OPENSEARCH_RECOVERY"])
        self.assertEqual("upgrade", env["JAEGER_DEMO_DEPLOY_MODE"])
        self.assertEqual(PLACEHOLDER_KEY, env["OCI_CLI_KEY_CONTENT"])
        self.assertIn("workflow_dispatch", document["on"])

    def test_untrusted_dispatch_does_not_reach_cloud_credentials(self):
        workflows = cloud_workflows()
        self.assertGreaterEqual(len(workflows), 3)
        with tempfile.TemporaryDirectory() as temp:
            for workflow in workflows:
                with self.subTest(workflow=workflow.name):
                    trace = run_dispatch(workflow, UNTRUSTED_REF, ATTACKER_SHA, Path(temp) / workflow.stem)
                    problems = trace.problems()
                    self.assertEqual(
                        [],
                        problems,
                        "manual dispatch of a non-default ref reached cloud credentials:\n"
                        + "\n".join(trace.events + problems),
                    )

    def test_checkout_uses_the_authorized_ref_not_a_different_sha(self):
        with tempfile.TemporaryDirectory() as temp:
            trace = run_dispatch(
                ROOT / ".github/workflows/ci-deploy-demo.yml",
                PROTECTED_REF,
                ATTACKER_SHA,
                Path(temp),
            )
            observed = trace.marker.read_text() if trace.marker.exists() else ""
            self.assertNotIn("OCI_CLI_KEY_CONTENT=set", observed, trace.events)
            self.assertTrue(trace.checkouts, "authorized deploy did not check out a ref:\n" + "\n".join(trace.events))
            for ref, commit in trace.checkouts:
                self.assertEqual(PROTECTED_REF, ref, trace.events)
                self.assertNotEqual(ATTACKER_SHA, commit, trace.events)

    def test_demo_cloud_job_is_bound_to_an_environment(self):
        document = load_workflow(ROOT / ".github/workflows/ci-deploy-demo.yml")
        for name, job in document["jobs"].items():
            text = yaml.dump(job)
            if "OCI_CLI_KEY_CONTENT" not in text and "OKE_CLUSTER_OCID" not in text:
                continue
            environment = job.get("environment")
            if isinstance(environment, dict):
                environment = environment.get("name")
            self.assertTrue(environment, f"job {name} uses the cloud credentials without an environment")

    def assert_reconcile_refuses(self, env):
        calls = []
        temps = []

        def record(args, **kwargs):
            child = kwargs.get("env") or {}
            key = "set" if child.get("OCI_CLI_KEY_CONTENT") else "absent"
            calls.append((list(args), key))
            return SimpleNamespace(stdout="", returncode=0)

        original_temp = tempfile.NamedTemporaryFile

        def record_temp(*args, **kwargs):
            temps.append("named-temp-file")
            return original_temp(*args, **kwargs)

        summary = Path(env["GITHUB_STEP_SUMMARY"])
        refused = False
        with patch("deploy.subprocess.run", record), patch("deploy.tempfile.NamedTemporaryFile", record_temp):
            try:
                deploy.reconcile(env)
            except ValueError:
                refused = True
        self.assertTrue(
            refused,
            "reconcile continued for an untrusted ref and ran "
            + ", ".join(f"{args[0]} key={key}" for args, key in calls),
        )
        self.assertEqual([], calls)
        self.assertEqual([], temps)
        self.assertFalse(summary.exists(), "step summary was written before refusal")

    def untrusted_env(self, directory, **extra):
        omit_event = extra.pop("omit_event", False)
        name = extra.pop("name", "summary")
        env = {
            "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_REF": UNTRUSTED_REF,
            "JAEGER_DEMO_DEPLOY_MODE": "upgrade",
            "JAEGER_DEMO_STACK": "oci",
            "JAEGER_OTEL_DEMO_DEPLOY_SCOPE": "jaeger",
            "JAEGER_OTEL_DEMO_OPENSEARCH_RECOVERY": "required",
            "OCI_CLI_KEY_CONTENT": PLACEHOLDER_KEY,
            "GITHUB_STEP_SUMMARY": str(Path(directory) / f"{name}.md"),
        }
        env.update(extra)
        if omit_event:
            env.pop("GITHUB_EVENT_NAME", None)
        return env

    def test_reconcile_refuses_untrusted_manual_dispatch_before_work(self):
        cases = [
            {"name": "upgrade"},
            {"name": "clean", "JAEGER_DEMO_DEPLOY_MODE": "clean"},
            {"name": "lookalike", "GITHUB_REF": "refs/heads/main-untrusted"},
            {"name": "capitalized", "GITHUB_REF": "refs/heads/Main"},
            {"name": "tag", "GITHUB_REF": "refs/tags/main"},
            {"name": "empty", "GITHUB_REF": ""},
            {"name": "omitted-event", "omit_event": True},
        ]
        with tempfile.TemporaryDirectory() as temp:
            for case in cases:
                with self.subTest(case=case["name"]):
                    self.assert_reconcile_refuses(self.untrusted_env(temp, **case))

    def test_reconcile_refuses_when_a_later_ref_read_says_main(self):
        with tempfile.TemporaryDirectory() as temp:
            env = RefChangesAfterFirstRead(self.untrusted_env(temp, name="reread"))
            self.assert_reconcile_refuses(env)

    def test_deploy_entry_point_refuses_before_a_child_process(self):
        modes = ("upgrade", "clean")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            for mode in modes:
                with self.subTest(mode=mode):
                    log_path = root / f"{mode}.log"
                    summary = root / f"{mode}-summary.md"
                    stubs = root / f"{mode}-stubs"
                    stubs.mkdir()
                    stub = textwrap.dedent(
                        """\
                        #!/usr/bin/env python3
                        import os
                        from pathlib import Path
                        key = "set" if os.environ.get("OCI_CLI_KEY_CONTENT") else "missing"
                        cluster = "set" if os.environ.get("OKE_CLUSTER_OCID") else "missing"
                        with open(os.environ["STUB_LOG"], "a", encoding="utf-8") as handle:
                            handle.write(f"{Path(__file__).name} key={key} cluster={cluster}\\n")
                        """
                    )
                    for name in ("kubectl", "bash", "helm"):
                        path = stubs / name
                        path.write_text(stub)
                        path.chmod(0o755)
                    child_env = os.environ.copy()
                    for name in CLOUD_SECRET_NAMES:
                        child_env.pop(name, None)
                    child_env.update({
                        "PATH": str(stubs) + os.pathsep + child_env.get("PATH", ""),
                        "GITHUB_EVENT_NAME": "workflow_dispatch",
                        "GITHUB_REF": UNTRUSTED_REF,
                        "JAEGER_DEMO_DEPLOY_MODE": mode,
                        "JAEGER_OTEL_DEMO_OPENSEARCH_RECOVERY": "required",
                        "OCI_CLI_KEY_CONTENT": PLACEHOLDER_KEY,
                        "GITHUB_STEP_SUMMARY": str(summary),
                        "STUB_LOG": str(log_path),
                        "HOME": str(root / f"{mode}-home"),
                    })
                    (root / f"{mode}-home").mkdir()
                    completed = subprocess.run(
                        [sys.executable, str(DEMO_DIR / "deploy.py")],
                        cwd=ROOT,
                        env=child_env,
                        text=True,
                        capture_output=True,
                        timeout=60,
                        check=False,
                    )
                    log = log_path.read_text() if log_path.exists() else ""
                    problems = []
                    if log.strip():
                        problems.append(log.strip())
                    if summary.exists():
                        problems.append("step summary was written")
                    if completed.returncode == 0:
                        problems.append("deploy.py exited 0")
                    self.assertEqual(
                        [],
                        problems,
                        "deploy.py reached a child process or wrote a file for an untrusted manual ref:\n"
                        + "\n".join(problems),
                    )

    def test_protected_ref_manual_dispatch_still_reaches_deploy_commands(self):
        calls = []

        def record(args, **_kwargs):
            calls.append(list(args))
            return SimpleNamespace(stdout="", returncode=0)

        env = {
            "GITHUB_EVENT_NAME": "workflow_dispatch",
            "GITHUB_REF": PROTECTED_REF,
            "JAEGER_DEMO_DEPLOY_MODE": "upgrade",
            "JAEGER_DEMO_STACK": "oci",
            "JAEGER_OTEL_DEMO_DEPLOY_SCOPE": "jaeger",
            "JAEGER_OTEL_DEMO_OPENSEARCH_RECOVERY": "required",
        }
        with patch("deploy.subprocess.run", record):
            deploy.reconcile(env)
        self.assertTrue(calls, "manual dispatch of the protected ref no longer deploys")
        self.assertEqual("kubectl", calls[0][0])


if __name__ == "__main__":
    unittest.main()
