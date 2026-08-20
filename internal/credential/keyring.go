package credential

import (
	"encoding/json"
	"errors"

	"github.com/zalando/go-keyring"
)

const keyringService = "techbeaver-beaver-cli"

type keyringStore struct{}

func openKeyring() Store {
	// Probing is the only reliable test: headless Linux has the library, no service.
	if err := keyring.Set(keyringService, "__probe__", "1"); err != nil {
		return nil
	}
	_ = keyring.Delete(keyringService, "__probe__")
	return &keyringStore{}
}

func (s *keyringStore) Describe() string { return "your system keychain" }

func (s *keyringStore) Save(profile string, tok *Token) error {
	encoded, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return keyring.Set(keyringService, sanitise(profile), string(encoded))
}

func (s *keyringStore) Load(profile string) (*Token, error) {
	raw, err := keyring.Get(keyringService, sanitise(profile))
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var tok Token
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func (s *keyringStore) Delete(profile string) error {
	err := keyring.Delete(keyringService, sanitise(profile))
	if err != nil && errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
