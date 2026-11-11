"""Retain recent failed pod records for each Helm release in one namespace."""

import collections
import datetime
import json
import os
import pathlib
import ssl
import urllib.error
import urllib.parse
import urllib.request


class Api:
    def __init__(self, endpoint, token, ca_file):
        self.endpoint = endpoint
        self.token = token
        self.context = ssl.create_default_context(cafile=ca_file)

    def request(self, path, method="GET", body=None):
        request = urllib.request.Request(
            self.endpoint + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers={
                "Authorization": "Bearer " + self.token,
                "Content-Type": "application/json",
            },
            method=method,
        )
        with urllib.request.urlopen(request, context=self.context, timeout=10) as response:
            return json.load(response)


def release_owner(api, namespace, pod, cache):
    """Follow controller ownership; pod instance labels need not be release names."""
    resources = {
        ("apps/v1", "Deployment"): "deployments",
        ("apps/v1", "ReplicaSet"): "replicasets",
        ("apps/v1", "StatefulSet"): "statefulsets",
        ("apps/v1", "DaemonSet"): "daemonsets",
        ("batch/v1", "Job"): "jobs",
        ("batch/v1", "CronJob"): "cronjobs",
    }
    metadata = pod["metadata"]
    visited = set()
    while True:
        annotations = metadata.get("annotations", {})
        release = annotations.get("meta.helm.sh/release-name")
        if release and annotations.get("meta.helm.sh/release-namespace") == namespace:
            return release
        owner = next(
            (owner for owner in metadata.get("ownerReferences", []) if owner.get("controller")),
            None,
        )
        if owner is None or owner["uid"] in visited:
            return None
        visited.add(owner["uid"])
        resource = resources.get((owner["apiVersion"], owner["kind"]))
        if resource is None:
            return None
        if owner["uid"] not in cache:
            path = "/apis/{}/namespaces/{}/{}/{}".format(
                owner["apiVersion"], namespace, resource,
                urllib.parse.quote(owner["name"], safe=""),
            )
            try:
                cache[owner["uid"]] = api.request(path)["metadata"]
            except urllib.error.HTTPError as error:
                if error.code != 404:
                    raise
                error.close()
                cache[owner["uid"]] = {}
        metadata = cache[owner["uid"]]
        if metadata.get("uid") != owner["uid"]:
            return None


def failure_time(pod):
    status = pod.get("status", {})
    timestamps = [pod["metadata"]["creationTimestamp"]]
    timestamps.extend(
        condition["lastTransitionTime"]
        for condition in status.get("conditions", [])
        if condition.get("lastTransitionTime")
    )
    for container in status.get("containerStatuses", []) + status.get("initContainerStatuses", []):
        finished = container.get("state", {}).get("terminated", {}).get("finishedAt")
        if finished:
            timestamps.append(finished)
    return max(datetime.datetime.fromisoformat(value.replace("Z", "+00:00")) for value in timestamps)


def prune(api, namespace, keep):
    if keep < 1:
        raise ValueError("failed pod retention must be at least one")
    groups = collections.defaultdict(list)
    cache = {}
    continuation = ""
    while True:
        query = urllib.parse.urlencode({
            "fieldSelector": "status.phase=Failed", "limit": 500, "continue": continuation,
        })
        page = api.request("/api/v1/namespaces/" + namespace + "/pods?" + query)
        for pod in page["items"]:
            if pod.get("status", {}).get("phase") != "Failed" or pod["metadata"].get("deletionTimestamp"):
                continue
            release = release_owner(api, namespace, pod, cache)
            if release:
                groups[release].append(pod)
        continuation = page.get("metadata", {}).get("continue", "")
        if not continuation:
            break
    for release, pods in sorted(groups.items()):
        pods.sort(key=lambda pod: (failure_time(pod), pod["metadata"]["name"]), reverse=True)
        print(f"{namespace}/{release}: retaining {min(keep, len(pods))} failed pods; pruning {max(0, len(pods) - keep)}", flush=True)
        for pod in pods[keep:]:
            metadata = pod["metadata"]
            path = "/api/v1/namespaces/{}/pods/{}".format(
                namespace, urllib.parse.quote(metadata["name"], safe=""),
            )
            try:
                api.request(path, "DELETE", {
                    "apiVersion": "v1", "kind": "DeleteOptions",
                    "preconditions": {"uid": metadata["uid"], "resourceVersion": metadata["resourceVersion"]},
                })
                print("Removed older failed pod " + metadata["name"], flush=True)
            except urllib.error.HTTPError as error:
                if error.code not in (404, 409):
                    raise
                error.close()


if __name__ == "__main__":
    credentials = pathlib.Path("/var/run/secrets/kubernetes.io/serviceaccount")
    host = os.environ["KUBERNETES_SERVICE_HOST"]
    if ":" in host:
        host = "[" + host + "]"
    port = os.environ["KUBERNETES_SERVICE_PORT_HTTPS"]
    api = Api("https://" + host + ":" + port, (credentials / "token").read_text().strip(), credentials / "ca.crt")
    prune(api, os.environ["POD_NAMESPACE"], int(os.environ["KEEP_FAILED_PODS"]))
