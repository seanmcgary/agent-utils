package store

import (
	"testing"
)

func TestDispatchCarriesCoAuthor(t *testing.T) {
	s := openTemp(t)
	id, err := s.CreateDispatch(Dispatch{
		Loop: "execution", Repo: "acme/widgets", Number: 5, Kind: KindStart,
		CoAuthor: "Seán O'Brien <7+sob@users.noreply.github.com>",
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDispatch(id)
	if err != nil {
		t.Fatal(err)
	}
	if d.CoAuthor != "Seán O'Brien <7+sob@users.noreply.github.com>" {
		t.Errorf("CoAuthor = %q", d.CoAuthor)
	}
}
