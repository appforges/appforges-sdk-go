package runtime

import (
	"encoding/base64"
	"fmt"
)

// EncodeUploadData encodes the public UploadAsset JSON data field. It performs
// no network request; multipart and pre-signed upload are not this contract.
func EncodeUploadData(data []byte) (string, error) {
	if len(data) >= 4*1024*1024 {
		return "", fmt.Errorf("small-file upload requires fewer than 4 MiB")
	}
	return base64.StdEncoding.EncodeToString(data), nil
}
