package store

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// ensure all manifest files resolve their paths against a representative
// document shape for that file.
func TestSecretFilesManifestCoversRealShapes(t *testing.T) {
	shapes := map[string]map[string]interface{}{
		"connections.json": {
			"groups": []interface{}{},
			"connections": []interface{}{
				map[string]interface{}{
					"id": "c1", "authType": "key",
					"password":   "key-passphrase",
					"keyContent": "",
				},
			},
		},
		"identities.json": {
			"identities": []interface{}{
				map[string]interface{}{"id": "i1", "password": "pw", "keyContent": "k"},
			},
		},
		"proxies.json": {
			"proxies": []interface{}{
				map[string]interface{}{"id": "p1", "pass": "pp"},
			},
		},
		"ai.json": {
			"models": []interface{}{
				map[string]interface{}{"id": "m1", "apiKey": "sk"},
			},
		},
		"settings.json": {
			"ai": map[string]interface{}{
				"models": []interface{}{
					map[string]interface{}{"id": "m1", "apiKey": "sk"},
				},
			},
		},
	}
	for file, obj := range shapes {
		if err := EncryptSecretFields(file, &obj, fakeCipherStore{}); err != nil {
			t.Fatalf("%s: EncryptSecretFields: %v", file, err)
		}
		if !ObjHasEncryptedSecrets(file, obj) {
			t.Fatalf("%s: expected enc:v1 residue after encrypt", file)
		}
		res, err := DecryptSecretFields(file, &obj, fakeCipherStore{})
		if err != nil {
			t.Fatalf("%s: DecryptSecretFields: %v", file, err)
		}
		if res.Decrypted == 0 {
			t.Fatalf("%s: expected at least one decrypted field", file)
		}
		if ObjHasEncryptedSecrets(file, obj) {
			t.Fatalf("%s: unexpected enc:v1 residue after decrypt", file)
		}
	}
}

func TestEncryptSecretFieldsFailsClosedWithoutCipher(t *testing.T) {
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "password": "secret"},
		},
	}
	if err := EncryptSecretFields("connections.json", &obj, nil); !errors.Is(err, ErrNoCipher) {
		t.Fatalf("expected ErrNoCipher, got %v", err)
	}
}

func TestEncryptSecretFieldsSkipsEmptyAndAlreadyEncrypted(t *testing.T) {
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "password": ""},
			map[string]interface{}{"id": "c2", "password": "enc:v1:already"},
		},
	}
	if err := EncryptSecretFields("connections.json", &obj, fakeCipherStore{}); err != nil {
		t.Fatal(err)
	}
	if obj["connections"].([]interface{})[1].(map[string]interface{})["password"] != "enc:v1:already" {
		t.Fatal("already-encrypted value must not be double-encrypted")
	}
}

func TestDecryptSecretFieldsCountsPlaintextForLazyMigration(t *testing.T) {
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "password": "enc:v1:" + base64.StdEncoding.EncodeToString([]byte("aaa"))},
			map[string]interface{}{"id": "c2", "password": "legacy-plaintext"},
		},
	}
	res, err := DecryptSecretFields("connections.json", &obj, fakeCipherStore{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decrypted != 1 || res.Plaintext != 1 {
		t.Fatalf("got %+v, want Decrypted=1 Plaintext=1", res)
	}
}

func TestClearSecretFieldsBlanksAllDeclaredPaths(t *testing.T) {
	obj := map[string]interface{}{
		"groups": []interface{}{},
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "password": "enc:v1:aaa", "keyContent": "enc:v1:bbb"},
		},
		"identities": []interface{}{
			map[string]interface{}{"id": "i1", "password": "enc:v1:ccc", "keyContent": "enc:v1:ddd"},
		},
	}
	// Applying every manifest file's paths to one object: the connections
	// paths clear the connection fields, the identities paths the identity
	// fields, and everything else no-ops.
	for _, sf := range SecretFiles {
		if err := ClearSecretFields(sf.File, obj); err != nil {
			t.Fatal(err)
		}
	}
	cm := obj["connections"].([]interface{})[0].(map[string]interface{})
	if cm["password"] != "" || cm["keyContent"] != "" {
		t.Fatalf("connection secrets not cleared: %v", cm)
	}
	im := obj["identities"].([]interface{})[0].(map[string]interface{})
	if im["password"] != "" || im["keyContent"] != "" {
		t.Fatalf("identity secrets not cleared: %v", im)
	}
}

func TestClearSecretFieldsClearsKeyContentToo(t *testing.T) {
	// Regression for the #1115 reset dead-end: clearSecretsInFile used to
	// miss connections[].keyContent, leaving residue that made Setup refuse
	// with "existing encrypted data" after a reset.
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "keyContent": "enc:v1:residue"},
		},
	}
	if err := ClearSecretFields("connections.json", obj); err != nil {
		t.Fatal(err)
	}
	cm := obj["connections"].([]interface{})[0].(map[string]interface{})
	if cm["keyContent"] != "" {
		t.Fatalf("keyContent not cleared: %v", cm)
	}
}

func TestDecryptStopsAtDecryptError(t *testing.T) {
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "password": "enc:v1:not-base64!!"},
		},
	}
	fc := fakeCipherStore{}
	if _, err := DecryptSecretFields("connections.json", &obj, fc); err == nil {
		t.Fatal("expected decrypt error for malformed ciphertext")
	}
}

func TestFieldRefSkipsNonStringLeaves(t *testing.T) {
	obj := map[string]interface{}{
		"connections": []interface{}{
			map[string]interface{}{"id": "c1", "port": 22, "password": "pw"},
		},
	}
	var refs []fieldRef
	collectFieldRefs(obj, strings.Split("connections.*.password", "."), &refs)
	if len(refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(refs))
	}
	// A path whose final segment lands on a non-string is not addressed.
	var none []fieldRef
	collectFieldRefs(obj, strings.Split("connections.*.port", "."), &none)
	if len(none) != 0 {
		t.Fatalf("expected 0 refs for non-string leaf, got %d", len(none))
	}
}

func TestUnknownFileIsNoOp(t *testing.T) {
	obj := map[string]interface{}{"password": "pw"}
	if err := EncryptSecretFields("unknown.json", &obj, fakeCipherStore{}); err != nil {
		t.Fatalf("expected no-op, got %v", err)
	}
	if res, err := DecryptSecretFields("unknown.json", &obj, fakeCipherStore{}); err != nil || res.Decrypted != 0 {
		t.Fatalf("expected no-op, got %+v err=%v", res, err)
	}
	if err := ClearSecretFields("unknown.json", obj); err != nil {
		t.Fatal(err)
	}
	if ObjHasEncryptedSecrets("unknown.json", obj) {
		t.Fatal("expected false for unknown file")
	}
}
