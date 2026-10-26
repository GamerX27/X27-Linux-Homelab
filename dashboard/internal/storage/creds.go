package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// SMB passwords live in root-only files that the unit points to with credentials=, since
// unit files are readable by everyone. The password is never sent back to the UI.

func credPath(name string) string { return filepath.Join(credDir, name+".cred") }

// credCheck refuses what would break the key=value lines of a credentials file.
func credCheck(username, password, domain string) error {
	if username == "" || len(username) > 128 {
		return errors.New("enter the username, or tick guest access")
	}
	if password == "" || len(password) > 256 {
		return errors.New("enter the password")
	}
	if strings.ContainsAny(username+password+domain, "\n\r\x00") {
		return errors.New("the username, password and domain can't contain line breaks")
	}
	return nil
}

func credText(username, password, domain string) (string, error) {
	if err := credCheck(username, password, domain); err != nil {
		return "", err
	}
	s := "username=" + username + "\npassword=" + password + "\n"
	if domain != "" {
		s += "domain=" + domain + "\n"
	}
	return s, nil
}

func writeCred(name, text string) error {
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		return err
	}
	return writeFile(credPath(name), text, 0o600)
}

func readCred(p string) (username, password, domain string) {
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "username":
			username = v
		case "password":
			password = v
		case "domain":
			domain = v
		}
	}
	return
}
