package security

import (
	"bytes"
	"testing"
)

func TestCodecRotationReadsOldAndSealsNew(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x71}, 32)
	newKey := bytes.Repeat([]byte{0x72}, 32)
	old, err := NewCodec(oldKey, "rotation-test")
	if err != nil {
		t.Fatal(err)
	}
	sealedOld, err := old.Seal("CONFIG_SECRET")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewCodecWithOldKeys(newKey, "rotation-test", [][]byte{oldKey})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := rotated.Open(sealedOld)
	if err != nil || plain != "CONFIG_SECRET" {
		t.Fatalf("old ciphertext unreadable: %q %v", plain, err)
	}
	if !rotated.NeedsRekey(sealedOld) {
		t.Fatal("old ciphertext should need rekey")
	}
	sealedNew, err := rotated.Rekey(sealedOld)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.NeedsRekey(sealedNew) {
		t.Fatal("rekeyed ciphertext still needs rekey")
	}
	if got, err := rotated.Open(sealedNew); err != nil || got != "CONFIG_SECRET" {
		t.Fatalf("new ciphertext unreadable: %q %v", got, err)
	}
}

func TestCodecRotationWithoutOldKeyFailsLoudly(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x81}, 32)
	newKey := bytes.Repeat([]byte{0x82}, 32)
	old, _ := NewCodec(oldKey, "rotation-test")
	sealed, _ := old.Seal("MUST_NOT_DISAPPEAR")
	rotated, _ := NewCodec(newKey, "rotation-test")
	if _, err := rotated.Open(sealed); err == nil {
		t.Fatal("★ missing old key silently accepted ciphertext")
	}
}
