package actions

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gobuffalo/buffalo"
	"github.com/pkg/errors"
	"golang.org/x/crypto/openpgp/armor"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

type secretName struct {
	Path     string `json:"path"`
	Username string `json:"username"`
}

// SecretsResource implements
// https://github.com/cpoppema/pass-server-node/blob/master/SPEC.rst when
// pointed to a password-store directory.
type SecretsResource struct {
	store string

	// Below are the encrypted values used for the responses generated by
	// the List and Show methods. These are populated by the Load method.
	sync.RWMutex
	loaded  bool
	index   string
	secrets map[secretName]string
}

// List implements POST /secrets/
func (v *SecretsResource) List(c buffalo.Context) error {
	var req struct{}
	err := c.Bind(&req)
	if err != nil {
		return v.error(c, http.StatusBadRequest, errors.Wrap(err, "unable to read request body"))
	}
	v.RLock()
	defer v.RUnlock()
	err = v.ensureLoaded()
	if err != nil {
		return v.error(c, http.StatusInternalServerError, errors.Wrap(err, "unable to load password store"))
	}
	var response struct {
		Response string `json:"response"`
	}
	response.Response = v.index
	return c.Render(200, r.JSON(response))
}

// Show implements POST /secret/
func (v *SecretsResource) Show(c buffalo.Context) error {
	var req secretName
	err := c.Bind(&req)
	if err != nil {
		return v.error(c, http.StatusBadRequest, errors.Wrap(err, "unable to read request body"))
	}
	if req.Path == "" {
		return v.error(c, http.StatusBadRequest, errors.New("no path found in request body"))
	}
	if req.Username == "" {
		return v.error(c, http.StatusBadRequest, errors.New("no username found in request body"))
	}
	var response struct {
		Response string `json:"response"`
	}
	v.RLock()
	defer v.RUnlock()
	if err = v.ensureLoaded(); err != nil {
		return v.error(c, http.StatusInternalServerError, errors.Wrap(err, "unable to load password store"))
	}
	var ok bool
	response.Response, ok = v.secrets[req]
	if !ok {
		return v.error(c, http.StatusBadRequest, errors.New("unknown secret"))
	}
	return c.Render(http.StatusOK, r.JSON(response))
}

// Load updates the values in v.index and v.secrets while holding the write
// lock.
func (v *SecretsResource) Load() error {
	filenames := make([]string, 0, 32)
	walkFn := func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Dir(path) == v.store {
			return nil
		}
		if match, _ := filepath.Match("*.gpg", info.Name()); match {
			filenames = append(filenames, path)
		}
		return nil
	}
	err := filepath.Walk(v.store, walkFn)
	if err != nil {
		return errors.Wrap(err, "issue while discovering secrets")
	}
	type item struct {
		Domain             string `json:"domain"`
		Path               string `json:"path"`
		Username           string `json:"username"`
		UsernameNormalized string `json:"username_normalized"`
	}
	index := make([]item, 0, 32)
	secrets := make(map[secretName]string)
	for _, filename := range filenames {
		secret := strings.TrimSuffix(strings.TrimPrefix(filename, v.store), ".gpg")
		username := path.Base(secret)
		secret = strings.TrimPrefix(path.Dir(secret), "/")
		domain := path.Base(secret)
		index = append(index, item{
			Domain:             domain,
			Path:               secret,
			Username:           username,
			UsernameNormalized: normalize(username),
		})
		text, err := readSecret(filename)
		if err != nil {
			return errors.Wrapf(err, "failed to read secret %s", filename)
		}
		secrets[secretName{
			Path:     secret,
			Username: username,
		}] = string(text)
	}
	ids, err := readIDs(v.store)
	if err != nil {
		return errors.Wrap(err, "unable to load .gpg-id")
	}
	args := []string{"--encrypt", "--armor"}
	for _, id := range ids {
		args = append(args, "-r", id)
	}
	indexJSON, err := json.Marshal(index)
	if err != nil {
		return errors.Wrap(err, "failed to create index")
	}
	buf := &bytes.Buffer{}
	cmd := exec.Command("gpg", args...)
	cmd.Stdin = bytes.NewReader(indexJSON)
	cmd.Stdout = buf
	err = cmd.Run()
	if err != nil {
		return errors.Wrap(err, "unable to run gpg command to encrypt index")
	}
	err = cmd.Wait()
	if err != nil {
		return errors.Wrap(err, "unable to encrypt index")
	}
	v.Lock()
	defer v.Unlock()
	v.index = buf.String()
	v.secrets = secrets
	v.loaded = true
	return nil
}

// ensureLoaded checks v.loaded, and calls Load if necessary. Call this
// function holding RLock.
func (v *SecretsResource) ensureLoaded() error {
	if !v.loaded {
		v.RUnlock()
		defer v.RLock()
		return v.Load()
	}
	return nil
}

func (v *SecretsResource) error(c buffalo.Context, status int, err error) error {
	var response struct {
		Error string `json:"error"`
	}
	response.Error = err.Error()
	return c.Render(status, r.JSON(response))
}

func readIDs(store string) ([]string, error) {
	ids := make([]string, 0, 5)
	f, err := os.Open(path.Join(store, ".gpg-id"))
	if err != nil {
		return nil, err
	}
	s := bufio.NewScanner(f)
	for s.Scan() {
		id := s.Text()
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	if s.Err() != nil {
		return ids, s.Err()
	}
	return ids, f.Close()
}

func readSecret(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open file")
	}
	defer f.Close()
	return enarmor(f)
}

func enarmor(r io.Reader) ([]byte, error) {
	buf := &bytes.Buffer{}
	w, err := armor.Encode(buf, "PGP MESSAGE", nil)
	if err != nil {
		return buf.Bytes(), errors.Wrap(err, "failed to create armor encoder")
	}
	_, err = io.Copy(w, r)
	if err != nil {
		return buf.Bytes(), errors.Wrap(err, "failed to copy to armored encoder")
	}
	err = w.Close()
	return buf.Bytes(), errors.Wrap(err, "failed to close armored encoder")
}

func normalize(s string) string {
	rs, n, err := transform.String(norm.NFKD, s)
	if err != nil {
		return ""
	}
	b := make([]byte, 0, n)
	for i := range rs {
		if rs[i] < 0x80 {
			b = append(b, rs[i])
		}
	}
	return string(b)
}
