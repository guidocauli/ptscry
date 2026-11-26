// ptscry-gui - Guido Cauli <guido.cauli@gmail.com>
// SPDX-License-Identifier: AGPL-3.0-or-later
// keys.go: the private keys the GUI keeps (imported copies, mode 0600) and
// the dialog that manages them.

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
)

const maxKeyFile = 64 << 10

type keyInfo struct {
	Name        string // file name in the key directory
	Path        string
	Fingerprint string // SHA256:..., "" when it cannot be read without the passphrase
	Kind        string // ssh-ed25519, ssh-rsa...
	Encrypted   bool
}

// keysDir is where imported keys live: ~/.config/ptscry/keys, private to the user.
func keysDir() (string, error) {
	cd, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(cd, "ptscry", "keys")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// inspectKey reads a private key file: it must be a private key (never a public
// one), at most 64 KiB; a key protected by a passphrase is fine.
func inspectKey(data []byte) (kind, fp string, enc bool, err error) {
	if len(data) == 0 || len(data) > maxKeyFile {
		return "", "", false, errors.New("non e' un file di chiave (dimensione)")
	}
	if !strings.Contains(string(data), "PRIVATE KEY") {
		if _, _, _, _, perr := ssh.ParseAuthorizedKey(data); perr == nil {
			return "", "", false, errors.New("questa e' una chiave PUBBLICA: serve il file della chiave privata")
		}
		return "", "", false, errors.New("non e' una chiave privata (manca l'intestazione PRIVATE KEY)")
	}
	signer, perr := ssh.ParsePrivateKey(data)
	if perr == nil {
		pk := signer.PublicKey()
		return pk.Type(), ssh.FingerprintSHA256(pk), false, nil
	}
	var pm *ssh.PassphraseMissingError
	if errors.As(perr, &pm) {
		if pm.PublicKey != nil {
			return pm.PublicKey.Type(), ssh.FingerprintSHA256(pm.PublicKey), true, nil
		}
		return "", "", true, nil
	}
	return "", "", false, fmt.Errorf("chiave non valida: %v", perr)
}

var badKeyChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// importKey copies the key file into the key directory (0600) and returns it.
func importKey(src string) (keyInfo, error) {
	st, err := os.Stat(src)
	if err != nil {
		return keyInfo{}, err
	}
	if !st.Mode().IsRegular() {
		return keyInfo{}, errors.New("non e' un file")
	}
	if st.Size() > maxKeyFile {
		return keyInfo{}, errors.New("file troppo grande per essere una chiave")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return keyInfo{}, err
	}
	kind, fp, enc, err := inspectKey(data)
	if err != nil {
		return keyInfo{}, err
	}
	dir, err := keysDir()
	if err != nil {
		return keyInfo{}, err
	}
	base := strings.Trim(badKeyChars.ReplaceAllString(filepath.Base(src), "_"), "._")
	if base == "" {
		base = "chiave"
	}
	name := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return keyInfo{}, err
	}
	return keyInfo{Name: name, Path: dst, Fingerprint: fp, Kind: kind, Encrypted: enc}, nil
}

// listKeys reads the imported keys.
func listKeys() []keyInfo {
	dir, err := keysDir()
	if err != nil {
		return nil
	}
	ents, _ := os.ReadDir(dir)
	var out []keyInfo
	for _, e := range ents {
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		kind, fp, enc, err := inspectKey(data)
		if err != nil {
			continue
		}
		out = append(out, keyInfo{Name: e.Name(), Path: p, Fingerprint: fp, Kind: kind, Encrypted: enc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// removeKey deletes an imported key (only inside the key directory).
func removeKey(name string) error {
	dir, err := keysDir()
	if err != nil {
		return err
	}
	if name != filepath.Base(name) || name == "." || name == ".." || name == "" {
		return errors.New("nome non valido")
	}
	return os.Remove(filepath.Join(dir, name))
}

// pickAndImport shows the system dialog and imports what was chosen.
func (g *gui) pickAndImport(done func(keyInfo)) {
	go func() {
		p, err := pickFile("Scegli il file della chiave privata", "")
		if err != nil {
			if !errors.Is(err, errPickCancelled) {
				g.flash("%v", err)
			}
			return
		}
		k, err := importKey(p)
		if err != nil {
			g.flash("%s: %v", filepath.Base(p), err)
			return
		}
		g.flashInfo("chiave %q importata", k.Name)
		done(k)
		g.invalidate()
	}()
}
