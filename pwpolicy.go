package main

import (
	_ "embed"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Password rules for passwords people choose in the app (POST /api/join, POST /api/password).
// The library is reachable from the internet, so a password has to survive guessing from there;
// the limiters slow guessing down, they can't save "1234567890". The command line
// (-set-password) only warns: it is the way back in for whoever runs the Pi.

const (
	minPwRunes = 10
	maxPwRunes = 256 // checked first: the pattern checks below take time that grows fast with the length
)

var (
	errPwShort = errors.New("Use at least 10 characters")
	errPwLong  = errors.New("Use at most 256 characters")
	errPwWeak  = errors.New("That password is too easy to guess. A few random words together work well.")
	errPwName  = errors.New("That password is too close to your name. A few random words together work well.")
)

//go:embed common_passwords.txt
var commonPwFile string

// commonPw is the embedded list (only entries of 10+ characters: shorter ones fail the length
// rule anyway). commonStems are its words that come with digits or symbols added ("password"
// from "password123"): with any other ending they are just as easy to guess.
var commonPw, commonStems = func() (map[string]bool, map[string]bool) {
	pws, stems := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(commonPwFile, "\n") {
		if l = strings.TrimSpace(l); l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		pws[l] = true
		if st := stemOf(l); st != l && utf8.RuneCountInString(st) >= 4 {
			stems[st] = true
		}
	}
	return pws, stems
}()

// stemOf drops the digits and symbols at the end.
func stemOf(s string) string {
	return strings.TrimRightFunc(s, func(r rune) bool { return !unicode.IsLetter(r) })
}

// pwPolicy says why pw is not good enough for the person called name, or nil.
func pwPolicy(pw, name string) error {
	if len(pw) > 4*maxPwRunes || utf8.RuneCountInString(pw) > maxPwRunes {
		return errPwLong
	}
	if utf8.RuneCountInString(pw) < minPwRunes {
		return errPwShort
	}
	low := strings.ToLower(pw)
	if guessable(low) {
		return errPwWeak
	}
	if aroundName(low, name) {
		return errPwName
	}
	return nil
}

// guessable (pw lowercased): a common password, also with other digits or symbols at the end;
// one or two runs of a simple pattern (aaaaaaaaaa, 1234567890, qwertzuiop, abcdef1234, and
// repetitions of these); or a short piece repeated (asdfasdfasdf, hahahahaha).
func guessable(low string) bool {
	stem := stemOf(low)
	if commonPw[low] || commonPw[stem] || commonStems[stem] {
		return true
	}
	rs := []rune(low)
	if patternRuns(rs) <= 2 {
		return true
	}
	if p := period(rs); p > 0 && (p < 6 || guessable(string(rs[:p]))) {
		return true
	}
	return false
}

// seqRows: what people type when they run a finger along the keyboard or count. Each row also
// counts backwards and wraps around (0 → 1, p → q), so repetitions like 12345678901234 are one run.
var seqRows = []string{
	"1234567890", "abcdefghijklmnopqrstuvwxyz",
	"qwertyuiop", "asdfghjkl", "zxcvbnm", // US keyboard
	"qwertzuiopü", "asdfghjklöä", "yxcvbnm", // German keyboard
	"!@#$%^&*()", "!\"§$%&/()=", // the number row with shift
}

var seqNext = func() map[[2]rune]bool {
	m := map[[2]rune]bool{}
	for _, row := range seqRows {
		rs := []rune(row)
		for i, r := range rs {
			n := rs[(i+1)%len(rs)]
			m[[2]rune{r, n}], m[[2]rune{n, r}] = true, true
		}
	}
	return m
}()

// patternRuns counts the stretches of rs in which each character repeats the one before it or
// is its neighbour in a seqRows row ("abcdef1234" is 2 runs).
func patternRuns(rs []rune) int {
	n := 1
	for i := 1; i < len(rs); i++ {
		if rs[i] != rs[i-1] && !seqNext[[2]rune{rs[i-1], rs[i]}] {
			n++
		}
	}
	return n
}

// period is the length of the shortest piece rs repeats (at least twice, the end may be cut
// short), or 0 if it isn't a repetition.
func period(rs []rune) int {
	for p := 1; p <= len(rs)/2; p++ {
		i := p
		for i < len(rs) && rs[i] == rs[i%p] {
			i++
		}
		if i == len(rs) {
			return p
		}
	}
	return 0
}

// aroundName (pw lowercased): the password is mostly the person's name ("Mama2024!!" or
// "jonas1234567890"): without the name, or a word of it with 3+ letters, fewer than 6
// characters are left, only something guessable, or only a few digits and symbols.
func aroundName(low, name string) bool {
	rest := low
	for _, part := range nameParts(name) {
		rest = strings.ReplaceAll(rest, part, "")
	}
	if rest == low {
		return false
	}
	n := utf8.RuneCountInString(rest)
	return n < 6 || guessable(rest) || (n < minPwRunes && strings.IndexFunc(rest, unicode.IsLetter) < 0)
}

// nameParts: the name, without its spaces, and its words, lowercased, those of 3+ letters, longest first.
func nameParts(name string) []string {
	low := strings.ToLower(strings.Join(strings.Fields(name), " "))
	cand := append([]string{low, strings.ReplaceAll(low, " ", "")}, strings.Fields(low)...)
	var out []string
	for _, c := range cand {
		if utf8.RuneCountInString(c) >= 3 {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}
