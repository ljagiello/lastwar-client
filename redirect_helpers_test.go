package main

import "testing"

func TestFindServerInfo(t *testing.T) {
	t.Run("nested under p", func(t *testing.T) {
		si := NewSFSObject()
		si.PutUtfString("ip", "1.2.3.4")
		p := NewSFSObject()
		p.PutSFSObject("serverInfo", si)
		content := NewSFSObject()
		content.PutSFSObject("p", p)
		got := findServerInfo(content)
		if got == nil || got.GetString("ip") != "1.2.3.4" {
			t.Fatalf("expected nested serverInfo to be found, got %v", got)
		}
	})
	t.Run("top-level fallback", func(t *testing.T) {
		si := NewSFSObject()
		si.PutUtfString("ip", "5.6.7.8")
		content := NewSFSObject()
		content.PutSFSObject("serverInfo", si)
		got := findServerInfo(content)
		if got == nil || got.GetString("ip") != "5.6.7.8" {
			t.Fatalf("expected top-level serverInfo to be found, got %v", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		content := NewSFSObject()
		if got := findServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	t.Run("nil content", func(t *testing.T) {
		if got := findServerInfo(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestGetIntFlexible(t *testing.T) {
	t.Run("numeric field", func(t *testing.T) {
		o := NewSFSObject()
		o.PutInt("port", 25092)
		if got := getIntFlexible(o, "port"); got != 25092 {
			t.Fatalf("got %d, want 25092", got)
		}
	})
	t.Run("string-numeric field", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("port", "17783")
		if got := getIntFlexible(o, "port"); got != 17783 {
			t.Fatalf("got %d, want 17783", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		o := NewSFSObject()
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
	t.Run("empty string", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("port", "")
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
}

func TestServerIDFromZone(t *testing.T) {
	cases := []struct{ zone, want string }{
		{"APS1234", "1234"},
		{"1234", "1234"},
		{"AP", "AP"},
		{"", ""},
	}
	for _, c := range cases {
		if got := serverIDFromZone(c.zone); got != c.want {
			t.Errorf("serverIDFromZone(%q) = %q, want %q", c.zone, got, c.want)
		}
	}
}
