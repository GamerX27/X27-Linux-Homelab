package auth

import (
	"errors"

	"github.com/msteinert/pam/v2"
)

// PAM authenticates against /etc/pam.d/dashboard (password-auth, like sshd), which
// also runs the account checks: expired or locked accounts are refused.
func PAM(username, password string) error {
	if username == "" || password == "" {
		return ErrDenied
	}
	tx, err := pam.StartFunc("dashboard", username, func(s pam.Style, msg string) (string, error) {
		switch s {
		case pam.PromptEchoOff, pam.PromptEchoOn:
			return password, nil
		case pam.ErrorMsg, pam.TextInfo:
			return "", nil
		}
		return "", errors.New("unsupported PAM conversation")
	})
	if err != nil {
		return err
	}
	defer tx.End()
	if err := tx.Authenticate(pam.DisallowNullAuthtok); err != nil {
		return err
	}
	return tx.AcctMgmt(pam.DisallowNullAuthtok)
}
