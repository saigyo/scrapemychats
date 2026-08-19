package browser

import (
	"slices"
	"strings"
	"testing"
)

// envFunc builds an environment lookup from a map, for injecting into
// candidatePaths.
func envFunc(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

func TestCandidatePathsDarwin(t *testing.T) {
	home := "/Users/tester"
	got := candidatePaths("darwin", envFunc(nil), home, ChannelChrome)
	want := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Users/tester/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Users/tester/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(darwin, chrome) =\n%q\nwant\n%q", got, want)
	}
}

func TestCandidatePathsDarwinPreferEdge(t *testing.T) {
	got := candidatePaths("darwin", envFunc(nil), "/Users/tester", ChannelEdge)
	want := []string{
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Users/tester/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Users/tester/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(darwin, msedge) =\n%q\nwant\n%q", got, want)
	}
}

func TestCandidatePathsDarwinNoHome(t *testing.T) {
	got := candidatePaths("darwin", envFunc(nil), "", ChannelChrome)
	want := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	if !slices.Equal(got, want) {
		t.Errorf("with no home, candidatePaths = %q, want %q", got, want)
	}
}

func TestCandidatePathsWindows(t *testing.T) {
	env := envFunc(map[string]string{
		"ProgramFiles":      `C:\Program Files`,
		"ProgramFiles(x86)": `C:\Program Files (x86)`,
		"LOCALAPPDATA":      `C:\Users\tester\AppData\Local`,
	})
	got := candidatePaths("windows", env, `C:\Users\tester`, "")
	want := []string{
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		`C:\Users\tester\AppData\Local\Google\Chrome\Application\chrome.exe`,
		"chrome.exe",
		"chrome",
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"msedge.exe",
		"msedge",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(windows, default) =\n%q\nwant\n%q", got, want)
	}
}

func TestCandidatePathsWindowsPreferEdge(t *testing.T) {
	env := envFunc(map[string]string{"ProgramFiles": `C:\Program Files`})
	got := candidatePaths("windows", env, `C:\Users\tester`, ChannelEdge)
	// Only ProgramFiles is set, so the (x86) and LocalAppData entries drop
	// out entirely instead of expanding to a bogus relative path.
	want := []string{
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"msedge.exe",
		"msedge",
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		"chrome.exe",
		"chrome",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(windows, msedge) =\n%q\nwant\n%q", got, want)
	}
}

// Windows environment variable names are case-insensitive; Go maps are not,
// so lookupEnv has to try the uppercase spelling too.
func TestCandidatePathsWindowsUppercaseEnv(t *testing.T) {
	env := envFunc(map[string]string{"PROGRAMFILES": `C:\Program Files`})
	got := candidatePaths("windows", env, "", ChannelChrome)
	if !slices.Contains(got, `C:\Program Files\Google\Chrome\Application\chrome.exe`) {
		t.Errorf("uppercase PROGRAMFILES not honoured: %q", got)
	}
}

func TestCandidatePathsLinux(t *testing.T) {
	got := candidatePaths("linux", envFunc(nil), "/home/tester", ChannelChrome)
	want := []string{
		"google-chrome", "google-chrome-stable", "chromium", "microsoft-edge",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(linux, chrome) = %q, want %q", got, want)
	}

	got = candidatePaths("linux", envFunc(nil), "/home/tester", ChannelEdge)
	want = []string{
		"microsoft-edge", "google-chrome", "google-chrome-stable", "chromium",
	}
	if !slices.Equal(got, want) {
		t.Errorf("candidatePaths(linux, msedge) = %q, want %q", got, want)
	}
}

// An unknown --browser-channel value must behave like the documented
// default rather than returning nothing.
func TestCandidatePathsUnknownPreferFallsBackToChrome(t *testing.T) {
	def := candidatePaths("darwin", envFunc(nil), "/Users/tester", "")
	odd := candidatePaths("darwin", envFunc(nil), "/Users/tester", "safari")
	if !slices.Equal(def, odd) {
		t.Errorf("unknown prefer changed the order: %q vs %q", odd, def)
	}
}

// Every candidate must be usable as-is: either absolute, or a bare program
// name for $PATH lookup. A stray relative path would silently resolve
// against the working directory.
func TestCandidatePathsAreAbsoluteOrBareNames(t *testing.T) {
	for _, goos := range []string{"darwin", "windows", "linux"} {
		env := envFunc(map[string]string{
			"ProgramFiles":      `C:\Program Files`,
			"ProgramFiles(x86)": `C:\Program Files (x86)`,
			"LOCALAPPDATA":      `C:\Users\tester\AppData\Local`,
		})
		for _, c := range candidatePaths(goos, env, "/home/tester", ChannelChrome) {
			bare := !strings.ContainsAny(c, `/\`)
			abs := strings.HasPrefix(c, "/") || (len(c) > 2 && c[1] == ':')
			if !bare && !abs {
				t.Errorf("%s: candidate %q is neither absolute nor a bare name", goos, c)
			}
		}
	}
}

func TestFindBrowserErrorMentionsBothBrowsers(t *testing.T) {
	// candidatePaths for an OS with no entries at all yields nothing, so
	// exercise the message directly through FindBrowser's formatting by
	// checking the sentinel and the required hints.
	_, err := FindBrowser("chrome")
	if err == nil {
		return // a real browser is installed on this machine; nothing to check
	}
	for _, want := range []string{"Chrome", "Edge", "http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("FindBrowser error %q does not mention %q", err, want)
		}
	}
}
