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

// BuilderSteps is where to paste the install message and how to publish,
// per builder: short, because the builder's own screen is right there.
// Button names vary between versions, so the usual alternatives are named.
func BuilderSteps(agent string) []string {
	switch agent {
	case "lovable":
		return []string{"Open your project in Lovable and paste the message into the chat. Send it.",
			"When Lovable has made the change, click Publish (top right), then Update.",
			"Open your live site once. Step 2 below turns green."}
	case "bolt":
		return []string{"Open your project in Bolt and paste the message into the chat. Send it.",
			"When Bolt has made the change, publish it again (the Publish or Deploy button, top right).",
			"Open your live site once. Step 2 below turns green."}
	case "replit":
		return []string{"Open your Repl and paste the message into the Agent (or Assistant) chat. Send it.",
			"When it's done, redeploy: the Deploy or Republish button.",
			"Open your live site once. Step 2 below turns green."}
	case "v0":
		return []string{"Open your chat in v0 and paste the message. Send it.",
			"When v0 has made the change, publish again (the Publish or Deploy button, top right).",
			"Open your live site once. Step 2 below turns green."}
	case "cursor":
		return []string{"Open your project in Cursor, start an Agent chat and paste the message. Send it.",
			"Review the change, commit it and deploy the way you usually do (for example push to GitHub).",
			"Open your live site once. Step 2 below turns green."}
	}
	return []string{"Paste the message into your AI agent's chat and send it.",
		"When it has made the change, publish or deploy your app the way you usually do.",
		"Open your live site once. Step 2 below turns green."}
}
