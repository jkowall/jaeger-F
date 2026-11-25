# Copyright (c) 2026 The Jaeger Authors.
# SPDX-License-Identifier: Apache-2.0

"""The public demo must not give anonymous callers cluster-admin on OpenSearch.

These tests fail while the OTel demo publishes OpenSearch Dashboards without
authentication and installs OpenSearch and Dashboards with their security
plugins disabled. They pass once that path is closed: the Dashboards ingress
is not applied, the ingress requires authentication, or those security plugins
are left enabled. A literal backend password must not be committed in the
Dashboards values while the ingress is public and unauthenticated.
"""

import re
import subprocess
import unittest
from pathlib import Path

import yaml

import deploy


DEMO = deploy.ROOT / "examples" / "otel-demo"
SCRIPT = DEMO / "deploy-all.sh"
INGRESS = DEMO / "ingress" / "ingress-opensearch.yaml"
OPENSEARCH_VALUES = DEMO / "opensearch-values.yaml"
DASHBOARD_VALUES = DEMO / "opensearch-dashboard-values.yaml"
JAEGER_CONFIG = DEMO / "jaeger-config.yaml"
WORKFLOW = deploy.ROOT / ".github" / "workflows" / "ci-deploy-demo.yml"

# Operations an anonymous caller can perform through the Dashboards console
# proxy while both security plugins are disabled. Each one is the same missing
# gate; index-management and other Dashboards plugin routes on that server are
# not called individually.
OPERATIONS = (
    ("DELETE", "jaeger-main*"),
    ("DELETE", "jaeger-archive*"),
    ("PUT", "_cluster/settings"),
    ("POST", "_bulk"),
)

AUTH_ANNOTATION_SUFFIXES = ("auth-type", "auth-url", "auth-secret", "auth-signin")


def extract_function(script, name):
    match = re.search(rf"(?m)^{re.escape(name)}\(\) \{{", script)
    if match is None:
        raise AssertionError(f"{name}() is not defined in deploy-all.sh")
    rest = script[match.end():]
    nxt = re.search(r"(?m)^[A-Za-z_][A-Za-z0-9_]*\(\) \{", rest)
    return rest[: nxt.start()] if nxt else rest


def route_enabled(mode, scope, predicate):
    result = subprocess.run(
        ["bash", "-c", 'source "$1"; MODE="$2"; DEPLOY_SCOPE="$3"; "$4"',
         "_", str(SCRIPT), mode, scope, predicate],
        text=True, capture_output=True)
    return result.returncode == 0


def ingress_document():
    if not INGRESS.is_file():
        return None
    return yaml.safe_load(INGRESS.read_text())


def ingress_targets_dashboards(ingress):
    if not ingress or ingress.get("kind") != "Ingress":
        return False
    spec = ingress.get("spec") or {}
    for rule in spec.get("rules") or []:
        paths = ((rule.get("http") or {}).get("paths")) or []
        for path in paths:
            backend = ((path.get("backend") or {}).get("service")) or {}
            port = (backend.get("port") or {}).get("number")
            if backend.get("name") == "opensearch-dashboards" and port == 5601 and path.get("path") == "/":
                return True
    return False


def ingress_has_auth(ingress):
    annotations = (ingress.get("metadata") or {}).get("annotations") or {}
    for key in annotations:
        suffix = key.split("/")[-1]
        if suffix in AUTH_ANNOTATION_SUFFIXES:
            return True
    return False


def script_publishes_unauthenticated_dashboards():
    script = SCRIPT.read_text()
    if not route_enabled("upgrade", "jaeger", "reconcile_ingress"):
        return False
    if "ingress/ingress-opensearch.yaml" not in extract_function(script, "deploy_ingress"):
        return False
    ingress = ingress_document()
    return ingress_targets_dashboards(ingress) and not ingress_has_auth(ingress)


def backend_security_disabled():
    script = SCRIPT.read_text()
    if "opensearch-values.yaml" not in extract_function(script, "deploy_opensearch_releases"):
        return False
    values = yaml.safe_load(OPENSEARCH_VALUES.read_text())
    config = (values.get("config") or {}).get("opensearch.yml") or ""
    if re.search(r"(?m)^\s*plugins\.security\.disabled:\s*true\s*$", config):
        return True
    for item in values.get("extraEnvs") or []:
        if item.get("name") == "DISABLE_SECURITY_PLUGIN" and str(item.get("value")).lower() == "true":
            return True
    return False


def dashboards_security_disabled():
    script = SCRIPT.read_text()
    if "opensearch-dashboard-values.yaml" not in extract_function(script, "deploy_opensearch_releases"):
        return False
    values = yaml.safe_load(DASHBOARD_VALUES.read_text())
    config = (values.get("config") or {}).get("opensearch_dashboards.yml") or ""
    return re.search(r"(?m)^\s*opensearch_security\.enabled:\s*false\s*$", config) is not None


def anonymous_cluster_admin():
    """True when the demo install gives an unauthenticated caller the operations above."""
    return (script_publishes_unauthenticated_dashboards()
            and backend_security_disabled()
            and dashboards_security_disabled())


def jaeger_index_prefixes():
    config = yaml.safe_load(JAEGER_CONFIG.read_text())
    backends = config["extensions"]["jaeger_storage"]["backends"]
    return tuple(backend["opensearch"]["indices"]["index_prefix"]
                 for backend in backends.values())


class PublicOpenSearchAdminTests(unittest.TestCase):
    def test_automatic_demo_deploy_reaches_the_opensearch_ingress(self):
        workflow = WORKFLOW.read_text()
        self.assertIn("python3 scripts/demo/deploy.py", workflow)
        self.assertIn("schedule:", workflow)
        desired = {name: deploy.fingerprint(name) for name in deploy.COMPONENTS}
        selected = deploy.selected_components("schedule", "oci", "jaeger", "upgrade", desired, desired)
        self.assertIn("otel-jaeger", selected)
        previous = dict(desired, opensearch="stale")
        changed = deploy.selected_components("push", "oci", "jaeger", "upgrade", previous, desired)
        self.assertIn("opensearch", changed)
        self.assertEqual(("jaeger-main", "jaeger-archive"), jaeger_index_prefixes())
        self.assertTrue(route_enabled("upgrade", "opensearch", "deploy_opensearch_stack"))

    def test_anonymous_caller_cannot_administer_the_cluster(self):
        exposed = anonymous_cluster_admin()
        for method, path in OPERATIONS:
            with self.subTest(method=method, path=path):
                self.assertFalse(
                    exposed,
                    f"anonymous {method} {path} is reachable through the public Dashboards ingress")

    def test_public_dashboards_does_not_inline_a_backend_password(self):
        if not script_publishes_unauthenticated_dashboards():
            return
        config = (yaml.safe_load(DASHBOARD_VALUES.read_text()).get("config") or {}).get(
            "opensearch_dashboards.yml") or ""
        if re.search(r"(?m)^\s*opensearch\.password:\s*\S+", config):
            self.fail("opensearch-dashboard-values.yaml sets opensearch.password inline "
                      "while Dashboards is published without authentication")


if __name__ == "__main__":
    unittest.main()
