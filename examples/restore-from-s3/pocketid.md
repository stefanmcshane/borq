# Restore an organization-managed Pocket ID provider

Follow this procedure for each managed Pocket ID provider that existed in the
original installation. External OIDC providers, such as Google Workspace, remain
with their provider; recover their original client configuration instead.
Complete the OpenBao, RustFS and management SQLite recovery stages in
[the recovery guide](README.md) first. Keep Borq and all original identity writers
stopped throughout this procedure.

## Recover the original identity and credentials

Read the `organization-pocketid` records from the closed recovered management
SQLite database. Retain the original organization ID, provider `id` and
`status.issuerUrl`. Do not generate a new provider ID or change the issuer URL.

Read version **1** of these paths from the recovered OpenBao KV mount:

- `secret/borq/organization-pocketid/ORGANIZATION_ID/PROVIDER_ID`: retain
  `encryptionKey`, `apiKey`, `clientSecret`, `ageIdentity` and `ageRecipient`.
- `secret/borq/organization-pocketid/ORGANIZATION_ID/PROVIDER_ID/backup`: retain
  `accessKey` and `secretKey`.

The credentials are in the record's `values` map. Borq reads version 1; writing
replacement credentials into a later version does not repair that identity.
Keep these values private, along with the original OIDC client records in the
management database. Do not rotate credentials during recovery.

## Rehydrate the fixed archive location

Fill in `pocketid-recovery.json` with this provider's original offsite source ID,
restricted repository recovery credentials and the new RustFS administrator credentials.
Set its destination bucket to `borq-pocketid-PROVIDER_ID` and use a unique
staging prefix such as `incident-unique/pocketid-staging`. This fixed bucket
must be fresh on the replacement store.

```sh
borq management backups list --config pocketid-recovery.json
borq management backups restore POCKET_ID_SNAPSHOT_ID --config pocketid-recovery.json --manifest pocketid-manifest.json
```

Use the full selected snapshot ID. Verify the completed manifest before proceeding.
The source archive retains its bucket-root `sqlite/` subtree. Restoring directly
under a destination prefix `sqlite` would incorrectly create `sqlite/sqlite/`.

With Python and `boto3` available on the recovery workstation, copy only the
verified SQLite subtree from staging to the fresh live prefix. This uses the
replacement administrator credentials in `pocketid-recovery.json`. It checks
manifest sizes and SHA-256 before upload and again after reading the live object.
Keep all writers stopped. The live prefix must initially be empty; on partial
failure, stop and review the partial destination before retrying.

Save this as `rehydrate-pocketid.py` outside the checkout:

```python
import hashlib
import json
import tempfile

import boto3
from botocore.config import Config

with open("pocketid-recovery.json") as stream:
    storage = json.load(stream)["destination"]
with open("pocketid-manifest.json") as stream:
    manifest = json.load(stream)
s3 = boto3.client("s3", endpoint_url=storage["endpoint"],
                  region_name=storage["region"],
                  aws_access_key_id=storage["accessKey"],
                  aws_secret_access_key=storage["secretKey"],
                  config=Config(s3={"addressing_style": "path"}))
bucket = storage["bucket"]
assert not s3.list_objects_v2(Bucket=bucket, Prefix="sqlite/", MaxKeys=1).get("Contents")
objects = [item for item in manifest["objects"] if item["key"].startswith("sqlite/")]
assert objects, "Snapshot has no Pocket ID SQLite objects"
for item in objects:
    key = item["key"]
    source = storage["prefix"].strip("/") + "/" + key
    with tempfile.TemporaryFile() as data:
        s3.download_fileobj(bucket, source, data)
        assert data.tell() == item["size"], key
        data.seek(0)
        assert hashlib.file_digest(data, "sha256").hexdigest() == item["sha256"], key
        data.seek(0)
        s3.upload_fileobj(data, bucket, key)
    with tempfile.TemporaryFile() as check:
        s3.download_fileobj(bucket, key, check)
        assert check.tell() == item["size"], key
        check.seek(0)
        assert hashlib.file_digest(check, "sha256").hexdigest() == item["sha256"], key
```

Run `python3 rehydrate-pocketid.py` with Python 3.11 or later, without
optimization so the checks remain active.
Leave the embedded vault checkpoint in staging; do not copy it into the live prefix.

Pocket ID uses `http://rustfs-svc.borq-mgmt.svc:9000` at runtime, including when
other workloads use external S3. Recover that Service and its storage first.

Through RustFS administration, recreate the **original scoped service account**
using the retained backup `accessKey` and `secretKey`. Give it only the original
bucket/prefix policy: bucket location and list access restricted to `sqlite` and
`sqlite/*`, plus GetObject, PutObject, DeleteObject, AbortMultipartUpload and
ListMultipartUploadParts on `arn:aws:s3:::borq-pocketid-PROVIDER_ID/sqlite/*`.
Verify reads with those credentials before restoring the database. A fresh root
account or newly generated scoped key does not match the retained vault record.

## Restore the database before starting Pocket ID

Privately edit `pocketid-restore.yaml`:

- Replace every `PROVIDER_ID` with the original 32-character provider ID, including
  resource names, labels and bucket names.
- Set the original issuer URL, encryption key and API key on the immutable
  runtime Secret. Set `archive-recipient-sha256` to the lowercase hex SHA-256 of
  the original `ageRecipient`, without a trailing newline in the hashed input.
- Set the temporary restore Secret's original backup credentials and age identity.
- Choose a suitable storage class on the fresh PVC if there is no default.

The YAML creates a fresh claim with the exact name Borq expects and a one-time
restore Pod. It does not start Pocket ID or a replication writer.

```sh
chmod 600 pocketid-restore.yaml
kubectl --context REPLACEMENT_CONTEXT apply -f pocketid-restore.yaml
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt wait --for=jsonpath='{.status.phase}'=Succeeded pod/pocketid-PROVIDER_ID-restore --timeout=300s
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt logs pocketid-PROVIDER_ID-restore
```

Require successful restoration of `/app/data/pocket-id.db`; investigate any
failure before retrying. Do not add flags that silently skip absent replicas or
existing databases. If multiple archive generations exist, select the required
generation from the captured manifest chronology. Add `-generation` and that
generation ID immediately after `restore` in the Pod's `args` before applying
the YAML. Rehydrated object timestamps do not preserve source chronology.

Restore `/app/data/uploads` from its independent file backup if used. SQLite
archives do not contain uploaded files.

Delete the temporary Pod, ConfigMap and credential Secret after success. Keep the
original runtime Secret and restored PVC:

```sh
kubectl --context REPLACEMENT_CONTEXT -n borq-mgmt delete pod/pocketid-PROVIDER_ID-restore configmap/pocketid-PROVIDER_ID-restore secret/pocketid-PROVIDER_ID-restore --wait=true
```

Continue with management resume in the [recovery guide](README.md#6-transfer-management-state-and-resume-one-writer).
Borq recreates its runtime archive Secret and configuration from the original
vault keys. Verify the original accounts, OIDC client and sign-in before traffic
cutover. Its normal Litestream sidecar replicates only; it cannot substitute for
the one-time restore above.
