package acts

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// corruptInPlace overwrites a stored artifact with the same bytes but one of them changed.
//
// The mutation happens **in the bucket, never through the plugin**, and that is the whole reason
// act 4 proves anything. Corruption injected through the code path under test can only ever
// exercise the branch somebody remembered to write; this is what bit rot actually looks like — same
// length, one byte different, deep enough inside that no header check would notice. Nothing but the
// checksum recorded when the backup was taken can catch it.
//
// The row is not touched. Its size and its checksum stay exactly as they were, which is the point:
// the evidence chain now says these bytes are not those bytes.
//
// The technique is lifted from test/conformance/corruption_test.go, which has been asserting the
// same thing per plugin since slice A6. The one difference is that this overwrites the original key
// rather than writing a copy, because the demo needs the backup Fleetward already has on record to
// become the bad one.
func corruptInPlace(ctx context.Context, cfg Config, bucket, key string) (int64, error) {
	client, err := minio.New(cfg.ObjectEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.ObjectAccessKey, cfg.ObjectSecretKey, ""),
		Secure: cfg.ObjectUseSSL,
	})
	if err != nil {
		return 0, fmt.Errorf("reach the object store at %s: %w", cfg.ObjectEndpoint, err)
	}

	object, err := client.GetObject(ctx, bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("read the artifact %s/%s: %w", bucket, key, err)
	}
	original, err := io.ReadAll(object)
	_ = object.Close()
	if err != nil {
		return 0, fmt.Errorf("read the artifact %s/%s: %w", bucket, key, err)
	}
	if len(original) == 0 {
		return 0, fmt.Errorf("the artifact %s/%s is empty, so corrupting it would prove nothing", bucket, key)
	}

	mutated := append([]byte(nil), original...)
	mutated[len(mutated)/2] ^= 0xFF
	if bytes.Equal(mutated, original) {
		return 0, fmt.Errorf("the mutation changed nothing, so act 4 would prove nothing")
	}

	if _, err := client.PutObject(ctx, bucket, key, bytes.NewReader(mutated), int64(len(mutated)),
		minio.PutObjectOptions{}); err != nil {
		return 0, fmt.Errorf("write the corrupted artifact back to %s/%s: %w", bucket, key, err)
	}
	return int64(len(mutated)), nil
}
