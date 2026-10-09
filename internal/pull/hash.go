package pull

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// SHA256File is the lowercase hex sha256 of the file at path.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the caller names the file to hash; reading that path is the purpose
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return SHA256Reader(f)
}

// SHA256Reader is the lowercase hex sha256 of everything r yields.
func SHA256Reader(r io.Reader) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
