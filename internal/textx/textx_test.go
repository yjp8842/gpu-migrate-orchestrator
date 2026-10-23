package textx

import "testing"

func TestQuote(t *testing.T) {
	cases := map[string]string{"": "''", "abc": "abc", "x y": "'x y'", "it's": `'it'"'"'s'`, "~/a": "'~/a'"}
	for in, want := range cases {
		if got := Quote(in); got != want {
			t.Fatalf("Quote(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRemotePathKeepsTilde(t *testing.T) {
	cases := map[string]string{"~/widgetrag-run/results.csv": "~/widgetrag-run/results.csv", "~/a b/c": "~/'a b/c'", "/tmp/x": "/tmp/x", "~": "~"}
	for in, want := range cases {
		if got := RemotePath(in); got != want {
			t.Fatalf("RemotePath(%q)=%q want %q", in, got, want)
		}
	}
}

func TestHeadTailRunes(t *testing.T) {
	if Head("가나다라", 2) != "가나" || Tail("가나다라", 2) != "다라" {
		t.Fatal("글자 단위가 아니다")
	}
	if MibToGib("15360") != 15 || MibToGib("23034") != 22 {
		t.Fatal(MibToGib("15360"), MibToGib("23034"))
	}
}
