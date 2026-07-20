package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

var (
	errCredentialUnsafe = errors.New("backend credential present")
	errDocumentTooLarge = errors.New("document exceeds limit")
)

func validateCredentialSafeText(data []byte, maxBytes int64, secrets ...string) error {
	if maxBytes >= 0 && int64(len(data)) > maxBytes {
		return errDocumentTooLarge
	}
	secretValues := normalizedSecrets(secrets)
	for _, secret := range secretValues {
		if bytes.Contains(data, []byte(secret)) {
			return fmt.Errorf("%w: secret literal", errCredentialUnsafe)
		}
	}
	if decoded, err := url.PathUnescape(string(data)); err == nil {
		for _, secret := range secretValues {
			if strings.Contains(decoded, secret) {
				return fmt.Errorf("%w: percent-decoded secret", errCredentialUnsafe)
			}
		}
	}
	return nil
}

func validateCredentialSafeJSON(data []byte, maxBytes int64, secrets ...string) error {
	if err := validateCredentialSafeText(data, maxBytes, secrets...); err != nil {
		return err
	}
	secretValues := normalizedSecrets(secrets)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, func(value string) error {
		for _, secret := range secretValues {
			if strings.Contains(value, secret) {
				return fmt.Errorf("%w: decoded JSON string", errCredentialUnsafe)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return requireRawJSONEOF(decoder)
}

func validateCredentialSafeHeaderValue(value string, secrets ...string) error {
	secretValues := normalizedSecrets(secrets)
	values := []string{value}
	if decoded, err := decodeJSONEscapes(value); err == nil {
		values = append(values, decoded)
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		values = append(values, decoded)
	}
	for _, candidate := range values {
		for _, secret := range secretValues {
			if strings.Contains(candidate, secret) {
				return fmt.Errorf("%w: header value", errCredentialUnsafe)
			}
		}
	}
	return nil
}

func validateCredentialSafeOpaqueJSON(data []byte, maxBytes int64, secrets ...string) error {
	if err := validateCredentialSafeText(data, maxBytes, secrets...); err != nil {
		return err
	}
	secretValues := normalizedSecrets(secrets)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanOpaqueJSONValue(decoder, func(value string) error {
		for _, secret := range secretValues {
			if strings.Contains(value, secret) {
				return fmt.Errorf("%w: decoded JSON string", errCredentialUnsafe)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return requireRawJSONEOF(decoder)
}

func decodeJSONEscapes(value string) (string, error) {
	var encoded strings.Builder
	encoded.Grow(len(value) + 2)
	encoded.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			encoded.WriteString(`\"`)
		case '\n':
			encoded.WriteString(`\n`)
		case '\r':
			encoded.WriteString(`\r`)
		case '\t':
			encoded.WriteString(`\t`)
		default:
			encoded.WriteRune(r)
		}
	}
	encoded.WriteByte('"')
	var decoded string
	if err := json.Unmarshal([]byte(encoded.String()), &decoded); err != nil {
		return "", err
	}
	return decoded, nil
}

func normalizedSecrets(secrets []string) []string {
	result := make([]string, 0, len(secrets))
	seen := make(map[string]struct{}, len(secrets))
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		if _, exists := seen[secret]; exists {
			continue
		}
		seen[secret] = struct{}{}
		result = append(result, secret)
	}
	return result
}
