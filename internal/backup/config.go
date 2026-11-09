package backup

import (
	"net/http"
	"time"

	"github.com/ottermq/otter_cloud/internal/config"
)

// putTimeout bounds one call to an s3 target, a whole upload included.
const putTimeout = time.Hour

// New returns the target and sealer the settings describe. The target is
// nil for none.
func New(c config.Backup) (Target, Sealer, error) {
	if c.Target == "none" || c.Target == "" {
		return nil, Sealer{}, nil
	}
	rs, err := ParseRecipients(c.Recipients)
	if err != nil {
		return nil, Sealer{}, err
	}
	sealer := Sealer{Recipients: rs}
	switch c.Target {
	case "fs":
		return FS{Dir: c.FSDir}, sealer, nil
	default:
		return &S3{Endpoint: c.S3Endpoint, Bucket: c.S3Bucket, Prefix: c.S3Prefix, Region: c.S3Region,
			AccessKey: c.S3AccessKey, SecretKey: c.S3SecretKey.Reveal(), Client: &http.Client{Timeout: putTimeout}}, sealer, nil
	}
}
