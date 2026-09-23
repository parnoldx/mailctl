package main

import (
	"flag"
	"strings"
	"testing"
)

func TestNormalizeAddrs(t *testing.T) {
	got, err := normalizeAddrs([]string{"Name <a@b.c>", "d@e.f"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a@b.c,d@e.f" {
		t.Errorf("normalizeAddrs = %v", got)
	}
	if _, err := normalizeAddrs([]string{"not an address"}); err == nil || !strings.Contains(err.Error(), "not an address") {
		t.Errorf("err = %v, want invalid-address usage error", err)
	}
}

func TestReorderArgs(t *testing.T) {
	for _, in := range [][]string{
		{"42", "--account", "x", "--read"},
		{"--account", "x", "42", "--read"},
		{"--read", "--account=x", "42"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		account := fs.String("account", "", "")
		read := fs.Bool("read", false, "")
		if err := fs.Parse(reorderArgs(in)); err != nil {
			t.Fatal(err)
		}
		if *account != "x" || !*read || fs.NArg() != 1 || fs.Arg(0) != "42" {
			t.Errorf("%v: account=%q read=%v args=%v", in, *account, *read, fs.Args())
		}
	}
}
