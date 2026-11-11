"""Exercise retention and API safeguards without deleting real pod history."""

import importlib.util
import io
import pathlib
import ssl
import unittest
import urllib.error
import urllib.parse
from unittest import mock


script = pathlib.Path(__file__).resolve().parents[2] / "values" / "pod-history.py"
spec = importlib.util.spec_from_file_location("pod_history", script)
history = importlib.util.module_from_spec(spec)
spec.loader.exec_module(history)


def owner(kind, name):
    return {"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": "uid-" + name, "controller": True}


def failed_pod(name, second, controller="alpha-controller", phase="Failed"):
    return {
        "metadata": {
            "name": name, "uid": "uid-" + name, "resourceVersion": "123",
            "creationTimestamp": "2026-10-23T10:00:00Z",
            # This deliberately differs from the actual Helm release name.
            "labels": {"app.kubernetes.io/instance": "shared-chart-platform"},
            "ownerReferences": [owner("ReplicaSet", controller + "-rs")],
        },
        "status": {"phase": phase, "containerStatuses": [{"state": {"terminated": {
            "finishedAt": f"2026-10-23T11:00:{second:02d}Z",
        }}}]},
    }


class Cluster:
    def __init__(self, pages):
        self.pages = pages
        self.deleted = []
        self.delete_errors = {}
        self.reads = []
        self.workloads = {}
        for controller, release in (("alpha-controller", "alpha"), ("alpha-webhook", "alpha"), ("beta-controller", "beta")):
            self.workloads["/apis/apps/v1/namespaces/platform/replicasets/" + controller + "-rs"] = {
                "metadata": {"uid": "uid-" + controller + "-rs", "ownerReferences": [owner("Deployment", controller)]},
            }
            self.workloads["/apis/apps/v1/namespaces/platform/deployments/" + controller] = {
                "metadata": {"uid": "uid-" + controller, "annotations": {
                    "meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": "platform",
                }},
            }

    def request(self, path, method="GET", body=None):
        if method == "DELETE":
            self.deleted.append((path.rsplit("/", 1)[1], body))
            code = self.delete_errors.get(path.rsplit("/", 1)[1])
            if code:
                raise urllib.error.HTTPError(path, code, "concurrent change", {}, None)
            return {}
        self.reads.append(path)
        parsed = urllib.parse.urlsplit(path)
        if parsed.path == "/api/v1/namespaces/platform/pods":
            query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)
            if query["fieldSelector"] != ["status.phase=Failed"]:
                raise AssertionError("pod list must filter failed records")
            page = int(query["continue"][0] or "0")
            return {"items": self.pages[page], "metadata": {"continue": str(page + 1) if page + 1 < len(self.pages) else ""}}
        return self.workloads[parsed.path]


class RetentionTest(unittest.TestCase):
    def test_five_failures_per_release_shared_across_components_and_versions(self):
        alpha = [failed_pod(f"alpha-{i}", i, "alpha-controller" if i % 2 == 0 else "alpha-webhook") for i in range(7)]
        # Preserve a long-lived pod that failed most recently, despite its older creation.
        alpha[0]["status"]["containerStatuses"][0]["state"]["terminated"]["finishedAt"] = "2026-10-23T12:00:00Z"
        beta = [failed_pod(f"beta-{i}", i, "beta-controller") for i in range(7)]
        cluster = Cluster([alpha + beta])
        # An older ReplicaSet still belongs to the same release across a rollout.
        old_path = "/apis/apps/v1/namespaces/platform/replicasets/alpha-controller-old"
        cluster.workloads[old_path] = {"metadata": {"uid": "uid-alpha-controller-old", "ownerReferences": [owner("Deployment", "alpha-controller")]}}
        alpha[2]["metadata"]["ownerReferences"] = [owner("ReplicaSet", "alpha-controller-old")]
        history.prune(cluster, "platform", 5)
        self.assertEqual({name for name, _ in cluster.deleted}, {"alpha-1", "alpha-2", "beta-0", "beta-1"})
        self.assertEqual(cluster.reads.count("/apis/apps/v1/namespaces/platform/deployments/alpha-controller"), 1)

    def test_running_successful_terminating_and_unowned_records_are_preserved(self):
        pods = [failed_pod(f"alpha-{i}", i) for i in range(6)]
        pods.extend([failed_pod("running", 20, phase="Running"), failed_pod("success", 20, phase="Succeeded")])
        unowned = failed_pod("unowned", 0)
        del unowned["metadata"]["ownerReferences"]
        pods.append(unowned)
        terminating = failed_pod("terminating", 20)
        terminating["metadata"]["deletionTimestamp"] = "2026-10-23T12:00:00Z"
        pods.append(terminating)
        cluster = Cluster([pods])
        history.prune(cluster, "platform", 5)
        self.assertEqual([name for name, _ in cluster.deleted], ["alpha-0"])

    def test_retention_combines_pages_before_selecting_oldest_records(self):
        pods = [failed_pod(f"alpha-{i}", i) for i in range(6)]
        cluster = Cluster([pods[3:], pods[:3]])
        history.prune(cluster, "platform", 5)
        self.assertEqual([name for name, _ in cluster.deleted], ["alpha-0"])

    def test_deletion_preconditions_protect_changed_or_recreated_pods(self):
        cluster = Cluster([[failed_pod(f"alpha-{i}", i) for i in range(8)]])
        cluster.delete_errors = {"alpha-2": 404, "alpha-1": 409}
        history.prune(cluster, "platform", 5)
        self.assertEqual([name for name, _ in cluster.deleted], ["alpha-2", "alpha-1", "alpha-0"])
        self.assertEqual(cluster.deleted[-1][1], {
            "apiVersion": "v1", "kind": "DeleteOptions",
            "preconditions": {"uid": "uid-alpha-0", "resourceVersion": "123"},
        })

    def test_missing_replaced_or_cyclic_owners_do_not_authorize_deletion(self):
        path = "/apis/apps/v1/namespaces/platform/replicasets/alpha-controller-rs"
        for mode in ("missing", "replaced", "cyclic", "other-namespace"):
            with self.subTest(mode=mode):
                cluster = Cluster([[failed_pod(f"alpha-{i}", i) for i in range(6)]])
                if mode == "missing":
                    original_request = cluster.request

                    def request(request_path, method="GET", body=None):
                        if request_path == path:
                            raise urllib.error.HTTPError(path, 404, "gone", {}, None)
                        return original_request(request_path, method, body)

                    cluster.request = request
                elif mode == "replaced":
                    cluster.workloads[path]["metadata"]["uid"] = "replacement"
                elif mode == "cyclic":
                    cluster.workloads[path]["metadata"]["ownerReferences"] = [owner("ReplicaSet", "alpha-controller-rs")]
                else:
                    cluster.workloads["/apis/apps/v1/namespaces/platform/deployments/alpha-controller"]["metadata"]["annotations"]["meta.helm.sh/release-namespace"] = "elsewhere"
                history.prune(cluster, "platform", 5)
                self.assertEqual(cluster.deleted, [])

    def test_invalid_retention_reaches_guard_before_cluster_access(self):
        api = mock.Mock()
        with self.assertRaisesRegex(ValueError, "at least one"):
            history.prune(api, "platform", 0)
        api.request.assert_not_called()

    def test_api_uses_verified_tls_authenticated_requests_and_delete_body(self):
        api = history.Api("https://cluster.test", "test-token", ssl.get_default_verify_paths().cafile)
        with mock.patch.object(history.urllib.request, "urlopen", return_value=io.BytesIO(b"{}")) as send:
            api.request("/api/v1/namespaces/platform/pods/old", "DELETE", {"preconditions": {"uid": "old-uid"}})
        request = send.call_args.args[0]
        self.assertEqual(request.full_url, "https://cluster.test/api/v1/namespaces/platform/pods/old")
        self.assertEqual(request.method, "DELETE")
        self.assertEqual(request.get_header("Authorization"), "Bearer test-token")
        self.assertEqual(request.data, b'{"preconditions": {"uid": "old-uid"}}')
        self.assertEqual(send.call_args.kwargs["timeout"], 10)
        self.assertEqual(send.call_args.kwargs["context"].verify_mode, ssl.CERT_REQUIRED)
