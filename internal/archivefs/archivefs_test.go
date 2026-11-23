package archivefs

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"a",
		"a/b",
		"a/b/c",
		"./a",
		"a/./b",
		"a/../b",        // cleaned to "b", stays inside the root
		"a/b/../../c",   // cleaned to "c"
		"..notanescape", // leading dots without separator are a plain name
		"foo/..bar/baz",
		"dir/",
		strings.Repeat("a", MaxEntryName),
	}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []struct {
		name string
		want error
	}{
		{"", ErrEmptyName},
		{strings.Repeat("a", MaxEntryName+1), ErrNameTooLong},
		{"/abs/path", ErrAbsolutePath},
		{"/", ErrAbsolutePath},
		{"C:/Windows/system32", ErrAbsolutePath},
		{"c:relative-drive", ErrAbsolutePath},
		{"//server/share", ErrAbsolutePath},
		{`\absolute\on\windows`, ErrAbsolutePath},
		{`C:\evil`, ErrAbsolutePath},
		{`\\server\share`, ErrAbsolutePath},
		{"..", ErrRootEscape},
		{"../evil", ErrRootEscape},
		{"a/../../evil", ErrRootEscape},
		{"a/../../../etc/passwd", ErrRootEscape},
		{`..\evil`, ErrRootEscape},
		{`a\..\..\evil`, ErrRootEscape},
	}
	for _, tc := range invalid {
		err := ValidateName(tc.name)
		if !errors.Is(err, tc.want) {
			t.Errorf("ValidateName(%q) = %v, want error wrapping %v", tc.name, err, tc.want)
		}
	}
}

type timeoutTestErr struct{ error }

func (timeoutTestErr) Timeout() bool { return true }

type temporaryTestErr struct{ error }

func (temporaryTestErr) Temporary() bool { return true }

func TestEntryError(t *testing.T) {
	base := errors.New("boom")
	err := EntryErrorf("dir/evil", base)

	var ee *EntryError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *EntryError, got %T", err)
	}
	if ee.Entry != "dir/evil" {
		t.Errorf("Entry = %q, want %q", ee.Entry, "dir/evil")
	}
	if ee.Op != "index" {
		t.Errorf("Op = %q, want %q", ee.Op, "index")
	}
	if !errors.Is(err, base) {
		t.Error("EntryError must unwrap to the underlying error")
	}
	if !strings.Contains(err.Error(), "dir/evil") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("error message %q must name the entry and the cause", err.Error())
	}

	// Timeout/Temporary delegation.
	te := EntryErrorf("x", timeoutTestErr{error: errors.New("slow")})
	if !te.(*EntryError).Timeout() {
		t.Error("Timeout should delegate to the wrapped error")
	}
	pe := EntryErrorf("x", temporaryTestErr{error: errors.New("flaky")})
	if !pe.(*EntryError).Temporary() {
		t.Error("Temporary should delegate to the wrapped error")
	}

	if EntryErrorf("x", nil) != nil {
		t.Error("EntryErrorf with nil error must return nil")
	}
}

func TestDuplicateEntryError(t *testing.T) {
	err := &DuplicateEntryError{Entry: "a//b", Path: "a/b"}
	msg := err.Error()
	if !strings.Contains(msg, "a//b") || !strings.Contains(msg, "a/b") {
		t.Errorf("error message %q must name both entries", msg)
	}
}
