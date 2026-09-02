# ADR-0007: S3-compatible object storage for backup artifacts

- **Status:** Accepted; the upload mechanism is superseded by
  [ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md)
- **Date:** 2026-07-26

> **Superseded in part, 2026-08-29.** The rule that plugins never hold storage credentials stands
> and is the load-bearing part of this ADR. The *mechanism* below — a single scoped presigned URL
> per upload — does not work: `PutObject` against a presigned URL requires `Content-Length`, which a
> streamed artifact does not know in advance. [ADR-0021](0021-plugins-upload-artifacts-as-multipart-parts.md)
> replaces it with a multipart upload in which core begins and completes the upload and the plugin
> writes parts through per-part grants. Presigned *download* for verification is unchanged.

## Context

Backup artifacts are large, immutable, written once and read rarely (but critically). They must
outlive the control plane, be storable off-host, and be retrievable by a sandbox container during
verification.

## Decision

S3-compatible object storage, accessed via `minio-go`, behind our own `ObjectStore` interface in
`internal/storage/objstore`. MinIO in the dev compose stack; AWS S3, GCS, R2, Ceph, or MinIO in
production.

- Plugins never hold long-lived storage credentials. Core issues scoped, time-limited presigned
  URLs for upload and download.
- Every artifact records a checksum at write time; verification re-checks it before restore.

## Consequences

- Backups survive the loss of the Fleetward host, which is the entire point of a backup.
- Presigned URLs keep the credential blast radius small and let plugins stream directly to storage
  without proxying bytes through core.
- The `ObjectStore` interface keeps a future filesystem or tape-adjacent backend possible.
- Cost: object storage is required even for a single-instance deployment. MinIO in compose makes
  this a non-issue for evaluation.

## Alternatives considered

- **Local filesystem.** Simplest, but a backup stored on the host it protects is not a backup.
- **Streaming through the control plane.** Makes core a bandwidth bottleneck and a memory-pressure
  risk during multi-gigabyte transfers.
