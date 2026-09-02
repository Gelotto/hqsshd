package cmd

import (
	"bytes"
	"testing"
)

func TestDetachScanner(t *testing.T) {
	cases := []struct {
		name   string
		reads  [][]byte
		send   []byte
		detach bool
	}{
		{"plain bytes pass through", [][]byte{[]byte("ls\r")}, []byte("ls\r"), false},
		{"ctrl-c passes through", [][]byte{{0x03}}, []byte{0x03}, false},
		{"prefix then d detaches", [][]byte{{0x02, 'd'}}, nil, true},
		{"chord split across reads", [][]byte{{'a', 0x02}, {'d', 'z'}}, []byte("a"), true},
		{"prefix twice sends one literal prefix", [][]byte{{0x02, 0x02}}, []byte{0x02}, false},
		{"prefix then other byte sends both", [][]byte{{0x02, 'x'}}, []byte{0x02, 'x'}, false},
		{"bytes before the chord are forwarded", [][]byte{[]byte("hi"), {0x02, 'd'}}, []byte("hi"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sc detachScanner
			var got []byte
			detached := false
			for _, r := range tc.reads {
				send, d := sc.scan(r)
				got = append(got, send...)
				if d {
					detached = true
					break
				}
			}
			if !bytes.Equal(got, tc.send) || detached != tc.detach {
				t.Errorf("send=%q detach=%v; want send=%q detach=%v", got, detached, tc.send, tc.detach)
			}
		})
	}
}
