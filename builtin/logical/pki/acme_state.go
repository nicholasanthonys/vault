package pki

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/vault/sdk/framework"
)

// How long nonces are considered valid.
const nonceExpiry = 15 * time.Minute

type acmeState struct {
	nextExpiry *atomic.Int64
	nonces     *sync.Map // map[string]time.Time
}

func NewACMEState() *acmeState {
	return &acmeState{
		nextExpiry: new(atomic.Int64),
		nonces:     new(sync.Map),
	}
}

func generateNonce() (string, error) {
	data := make([]byte, 21)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", err
	}

	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (a *acmeState) GetNonce() (string, time.Time, error) {
	now := time.Now()
	nonce, err := generateNonce()
	if err != nil {
		return "", now, err
	}

	then := now.Add(nonceExpiry)
	a.nonces.Store(nonce, then)

	nextExpiry := a.nextExpiry.Load()
	next := time.Unix(nextExpiry, 0)
	if now.After(next) || then.Before(next) {
		a.nextExpiry.Store(then.Unix())
	}

	return nonce, then, nil
}

func (a *acmeState) RedeemNonce(nonce string) bool {
	rawTimeout, present := a.nonces.LoadAndDelete(nonce)
	if !present {
		return false
	}

	timeout := rawTimeout.(time.Time)
	if time.Now().After(timeout) {
		return false
	}

	return true
}

func (a *acmeState) DoTidyNonces() {
	now := time.Now()
	expiry := a.nextExpiry.Load()
	then := time.Unix(expiry, 0)

	if expiry == 0 || now.After(then) {
		a.TidyNonces()
	}
}

func (a *acmeState) TidyNonces() {
	now := time.Now()
	nextRun := now.Add(nonceExpiry)

	a.nonces.Range(func(key, value any) bool {
		timeout := value.(time.Time)
		if now.After(timeout) {
			a.nonces.Delete(key)
		}

		if timeout.Before(nextRun) {
			nextRun = timeout
		}

		return false /* don't quit looping */
	})

	a.nextExpiry.Store(nextRun.Unix())
}

func (a *acmeState) CreateAccount(c *jwsCtx, contact []string, termsOfServiceAgreed bool) (map[string]interface{}, error) {
	// TODO
	return nil, nil
}

func (a *acmeState) LoadAccount(keyID string) (map[string]interface{}, error) {
	// TODO
	return nil, nil
}

func (a *acmeState) DoesAccountExist(keyId string) bool {
	account, err := a.LoadAccount(keyId)
	return err == nil && len(account) > 0
}

func (a *acmeState) LoadJWK(keyID string) ([]byte, error) {
	key, err := a.LoadAccount(keyID)
	if err != nil {
		return nil, err
	}

	jwk, present := key["jwk"]
	if !present {
		return nil, fmt.Errorf("malformed key entry lacks JWK")
	}

	return jwk.([]byte), nil
}

func (a *acmeState) ParseRequestParams(data *framework.FieldData) (*jwsCtx, map[string]interface{}, error) {
	var c jwsCtx
	var m map[string]interface{}

	// Parse the key out.
	rawJWKBase64, ok := data.GetOk("protected")
	if !ok {
		return nil, nil, fmt.Errorf("missing required field 'protected': %w", ErrMalformed)
	}
	jwkBase64 := rawJWKBase64.(string)

	jwkBytes, err := base64.RawURLEncoding.DecodeString(jwkBase64)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to base64 parse 'protected': %s: %w", err, ErrMalformed)
	}
	if err = c.UnmarshalJSON(a, jwkBytes); err != nil {
		return nil, nil, fmt.Errorf("failed to json unmarshal 'protected': %w", err)
	}

	// Since we already parsed the header to verify the JWS context, we
	// should read and redeem the nonce here too, to avoid doing any extra
	// work if it is invalid.
	if !a.RedeemNonce(c.Nonce) {
		return nil, nil, fmt.Errorf("invalid or reused nonce: %w", ErrBadNonce)
	}

	rawPayloadBase64, ok := data.GetOk("payload")
	if !ok {
		return nil, nil, fmt.Errorf("missing required field 'payload': %w", ErrMalformed)
	}
	payloadBase64 := rawPayloadBase64.(string)

	rawSignatureBase64, ok := data.GetOk("signature")
	if !ok {
		return nil, nil, fmt.Errorf("missing required field 'signature': %w", ErrMalformed)
	}
	signatureBase64 := rawSignatureBase64.(string)

	// go-jose only seems to support compact signature encodings.
	compactSig := fmt.Sprintf("%v.%v.%v", jwkBase64, payloadBase64, signatureBase64)
	m, err = c.VerifyJWS(compactSig)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to verify signature: %w", err)
	}

	return &c, m, nil
}
