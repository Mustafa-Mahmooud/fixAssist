package normalize

import (
	"regexp"
	"strings"
)

// JS fingerprint v2 (Phase 3). Phase 1 showed that v1 merges different bugs
// that share a message on one route: in minified bundles every crash-side
// frame is dropped, and quoted property names became <str>. v2 keeps two
// code-level signals that survive minification:
//
//  1. identifiers quoted by the JS engine in its own error messages
//     ("reading 'profile'", "evaluating 'a.b'", "can't access property \"x\"");
//     these are code, not user data;
//  2. the nearest non-minified function name to the crash site
//     (object-property names like onClick or run survive minification).
//
// Fingerprints carry "v2|js|", so v1 issues are never silently merged.

// Engine message shapes whose quoted part is an identifier path.
var reEngineIdent = []*regexp.Regexp{
	regexp.MustCompile(`\(reading '([A-Za-z_$][\w$]*)'\)`),                                   // Chrome
	regexp.MustCompile(`\(evaluating '([A-Za-z_$][\w$.]*)'\)`),                               // Safari
	regexp.MustCompile(`can't access property "([A-Za-z_$][\w$]*)"`),                         // Firefox
	regexp.MustCompile(`Cannot (?:read|set) properties of \w+ \(\w+ '([A-Za-z_$][\w$]*)'\)`), // Chrome variants
	regexp.MustCompile(`'([A-Za-z_$][\w$.]*)' is not a function`),
	regexp.MustCompile(`^([A-Za-z_$][\w$.]*) is not (?:a function|defined)`),
}

// MessageV2 is Message, except engine-quoted identifiers are kept.
func MessageV2(s string) string {
	type keep struct{ token, ident string }
	var kept []keep
	for _, re := range reEngineIdent {
		s = re.ReplaceAllStringFunc(s, func(m string) string {
			sub := re.FindStringSubmatchIndex(m)
			start, end := sub[2], sub[3]
			// Swallow the surrounding quotes too, or the generic
			// quoted-literal rule would still turn them into <str>.
			if start > 0 && end < len(m) && (m[start-1] == '\'' || m[start-1] == '"') && m[end] == m[start-1] {
				start--
				end++
			}
			// Tokens are letters only, so no Message rule touches them.
			tok := "QXV" + strings.Repeat("Q", len(kept)+1) + "QXV"
			kept = append(kept, keep{tok, m[start:end]})
			return m[:start] + tok + m[end:]
		})
	}
	s = Message(s)
	// Reverse order: a later pattern may have wrapped an earlier token.
	for i := len(kept) - 1; i >= 0; i-- {
		s = strings.Replace(s, kept[i].token, kept[i].ident, 1)
	}
	return s
}

// frameworkNames are non-minified names that belong to frameworks or the
// browser, not to the app, and so carry no grouping signal.
var frameworkNames = map[string]bool{
	"dispatchEvent": true, "callCallback": true, "invokeGuardedCallback": true,
	"invokeGuardedCallbackImpl": true, "batchedUpdates": true, "processDispatchQueue": true,
	"handleEvent": true, "apply": true, "call": true, "next": true, "step": true,
	"fulfilled": true, "rejected": true, "Promise": true, "new Promise": true,
}

// NearestNamedFrame is the closest in-app frame to the crash whose function
// name survived minification, or "".
func NearestNamedFrame(e JSError) string {
	origin := Origin(e.PageURL)
	for i := len(e.Frames) - 1; i >= 0; i-- {
		f := e.Frames[i]
		inApp := origin != "" && strings.HasPrefix(f.Filename, origin)
		if origin == "" && f.InApp != nil {
			inApp = *f.InApp
		}
		if !inApp || LooksMinified(f.Function) {
			continue
		}
		n := shortName(f.Function)
		if frameworkNames[n] {
			continue
		}
		return n
	}
	return ""
}

// JSFingerprintV2 hashes type + v2 message + nearest named frame + route.
func JSFingerprintV2(e JSError) string {
	return "v2:" + hashHex("v2|js|"+e.Type+"|"+MessageV2(e.Message)+"|"+NearestNamedFrame(e)+"|"+RouteTemplate(e.PageURL))
}

// JSSignatureV2 is the readable form used for titles and embeddings.
func JSSignatureV2(e JSError, t Tags) string {
	s := "js " + e.Type + " \"" + MessageV2(e.Message) + "\" route=" + RouteTemplate(e.PageURL)
	if fn := NearestNamedFrame(e); fn != "" {
		s += " fn=" + fn
	}
	return s + t.suffix()
}
