package web

import "fmt"

// AgentNames are display names for projects.Agents.
var AgentNames = map[string]string{
	"lovable": "Lovable", "bolt": "Bolt", "cursor": "Cursor", "replit": "Replit", "v0": "v0", "other": "your AI agent",
}

// BackendNames are display names for projects.Backends.
var BackendNames = map[string]string{"supabase": "Supabase", "firebase": "Firebase", "unknown": "Other"}

func agentName(a string) string {
	if n, ok := AgentNames[a]; ok {
		return n
	}
	return a
}

func backendName(b string) string {
	if n, ok := BackendNames[b]; ok {
		return n
	}
	return b
}

// ScriptTag is the one line a founder installs.
func ScriptTag(ingestBase, publicKey string) string {
	return fmt.Sprintf(`<script data-fix-sdk src="%s/sdk/%s.js"></script>`, ingestBase, publicKey)
}

// InstallPrompt is pasted into the founder's AI agent. It asks for one small,
// exact change and how to check it, the same rules as fix prompts.
func InstallPrompt(agent, tag string) string {
	switch agent {
	case "cursor", "other":
		return "Add an error-monitoring script to this web app.\n\n" +
			"Insert this exact line as the first <script> inside <head> of the root HTML file " +
			"(index.html for Vite or plain HTML apps). For a Next.js app, add it in app/layout.tsx " +
			"(or pages/_document.tsx) inside <head>, as a plain <script> tag, not next/script, so it loads before the app code.\n\n" +
			tag + "\n\n" +
			"Copy it exactly as written. Don't add async or defer, don't load it from a component, and don't change any other code.\n" +
			"To check: open the app, view the page source, and confirm the line is inside <head> above the other scripts."
	case "v0":
		return "Add an error-monitoring script to this app.\n\n" +
			"In app/layout.tsx, add this exact line inside <head> as a plain <script> tag (not next/script), above any other scripts:\n\n" +
			tag + "\n\n" +
			"Copy it exactly as written, don't add async or defer, and don't change anything else.\n" +
			"To check: open the published app, view the page source, and confirm the line is inside <head>."
	default: // lovable, bolt, replit: product-level wording
		return "Please add an error-monitoring script to my app.\n\n" +
			"Put this exact line in index.html, inside <head>, as the very first <script> tag (above all other scripts):\n\n" +
			tag + "\n\n" +
			"Copy it exactly as written. Don't wrap it in a component, don't add async or defer, and don't change anything else in the app.\n" +
			"To check: after publishing, open the app, view the page source, and confirm the line is inside <head> above the other scripts."
	}
}
