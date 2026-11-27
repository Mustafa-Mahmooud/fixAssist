package alerts

import (
	"bytes"
	"html/template"
	"strings"
)

// Email is the content of one branded HTML email. Every field is optional
// except Heading; RenderHTML lays them out in a fixed order:
// label, heading, paragraphs, stats, items, code block, buttons, note.
type Email struct {
	Preheader string // inbox preview text (hidden in the body)
	Label     string // small uppercase tag above the heading, e.g. "New issue"
	Tone      string // "", "bad", "warn" or "ok": colours the label
	Heading   string
	Paras     []string
	Stats     []Stat
	Items     []Item
	CodeLabel string // e.g. "Paste this into Lovable"
	Code      string
	Buttons   []Button // the first is the primary button
	Note      string   // small grey text under the buttons
	Footer    string   // defaults to the alert footer
}

type Stat struct{ Label, Value string }

type Item struct {
	Title, Meta, URL string
	Badge            string
}

type Button struct{ Label, URL string }

// Colours mirror the dashboard tokens in web/static/app.css. Email clients
// ignore CSS variables and most <style> rules, so everything is inline.
var emailTmpl = template.Must(template.New("email").Funcs(template.FuncMap{
	"toneColor": func(t string) template.CSS {
		switch t {
		case "bad":
			return "#c62828;background:#fdecec"
		case "warn":
			return "#b45309;background:#fdf3e4"
		case "ok":
			return "#15803d;background:#e8f6ee"
		}
		return "#4f46e5;background:#eef0ff"
	},
	"badgeColor": func(b string) template.CSS {
		switch strings.ToLower(b) {
		case "critical", "high":
			return "#c62828;background:#fdecec"
		case "medium":
			return "#b45309;background:#fdf3e4"
		}
		return "#646a76;background:#eff1f4"
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light">
<meta name="supported-color-schemes" content="light">
<title>{{.Heading}}</title>
</head>
<body style="margin:0;padding:0;background:#f5f6f8;-webkit-text-size-adjust:100%;">
{{if .Preheader}}<div style="display:none;max-height:0;overflow:hidden;opacity:0;color:transparent;font-size:1px;line-height:1px;">{{.Preheader}}&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;&#8203;&nbsp;</div>{{end}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f5f6f8;">
<tr><td align="center" style="padding:32px 16px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#16181d;">
<tr><td style="padding:0 4px 20px;">
<table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>
<td style="width:30px;height:30px;background:#4f46e5;border-radius:8px;color:#ffffff;font-size:12px;font-weight:800;text-align:center;vertical-align:middle;">FA</td>
<td style="padding-left:10px;font-size:15px;font-weight:700;color:#16181d;letter-spacing:-0.01em;">Fix Assistant</td>
</tr></table>
</td></tr>
<tr><td style="background:#ffffff;border:1px solid #e4e6eb;border-radius:12px;padding:32px;">
{{if .Label}}<div style="margin:0 0 12px;"><span style="display:inline-block;padding:3px 10px;border-radius:999px;font-size:11px;font-weight:700;letter-spacing:.04em;text-transform:uppercase;color:{{toneColor .Tone}};">{{.Label}}</span></div>{{end}}
<h1 style="margin:0 0 16px;font-size:22px;line-height:1.3;font-weight:700;color:#16181d;letter-spacing:-0.01em;">{{.Heading}}</h1>
{{range .Paras}}<p style="margin:0 0 14px;font-size:15px;line-height:1.6;color:#3a3f4a;">{{.}}</p>{{end}}
{{if .Stats}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:8px 0 20px;"><tr>
{{range $i, $s := .Stats}}<td style="{{if $i}}border-left:8px solid #ffffff;{{end}}background:#f9fafb;border-radius:8px;padding:14px 12px;text-align:center;">
<div style="font-size:22px;font-weight:700;color:#16181d;">{{$s.Value}}</div>
<div style="font-size:12px;color:#646a76;margin-top:2px;">{{$s.Label}}</div>
</td>{{end}}
</tr></table>{{end}}
{{if .Items}}<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:4px 0 20px;border-top:1px solid #e4e6eb;">
{{range .Items}}<tr><td style="padding:14px 0;border-bottom:1px solid #e4e6eb;">
<div style="font-size:15px;font-weight:600;line-height:1.4;">{{if .URL}}<a href="{{.URL}}" style="color:#16181d;text-decoration:none;">{{.Title}}</a>{{else}}{{.Title}}{{end}}{{if .Badge}} <span style="display:inline-block;padding:1px 8px;border-radius:999px;font-size:11px;font-weight:700;vertical-align:1px;color:{{badgeColor .Badge}};">{{.Badge}}</span>{{end}}</div>
{{if .Meta}}<div style="font-size:13px;line-height:1.5;color:#646a76;margin-top:3px;">{{.Meta}}</div>{{end}}
</td></tr>{{end}}
</table>{{end}}
{{if .Code}}{{if .CodeLabel}}<p style="margin:8px 0 8px;font-size:13px;font-weight:600;color:#16181d;">{{.CodeLabel}}</p>{{end}}
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="margin:0 0 20px;"><tr><td style="background:#f3f4f6;border:1px solid #e4e6eb;border-radius:8px;padding:14px 16px;">
<pre style="margin:0;white-space:pre-wrap;word-break:break-word;font-family:SFMono-Regular,Consolas,'Liberation Mono',Menlo,monospace;font-size:13px;line-height:1.55;color:#16181d;">{{.Code}}</pre>
</td></tr></table>{{end}}
{{if .Buttons}}<table role="presentation" cellpadding="0" cellspacing="0" border="0" style="margin:8px 0 4px;"><tr>
{{range $i, $b := .Buttons}}<td style="padding:0 8px 8px 0;">{{if eq $i 0}}<a href="{{$b.URL}}" style="display:inline-block;background:#4f46e5;color:#ffffff;font-size:15px;font-weight:600;text-decoration:none;padding:12px 22px;border-radius:8px;">{{$b.Label}}</a>{{else}}<a href="{{$b.URL}}" style="display:inline-block;background:#ffffff;color:#16181d;border:1px solid #d3d6dd;font-size:14px;font-weight:600;text-decoration:none;padding:11px 18px;border-radius:8px;">{{$b.Label}}</a>{{end}}</td>{{end}}
</tr></table>{{end}}
{{if .Note}}<p style="margin:16px 0 0;font-size:13px;line-height:1.55;color:#646a76;">{{.Note}}</p>{{end}}
</td></tr>
<tr><td style="padding:20px 4px 0;font-size:12px;line-height:1.5;color:#8b909b;text-align:center;">
{{if .Footer}}{{.Footer}}{{else}}You get this because you're a member of this app in Fix Assistant.{{end}}
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`))

// RenderHTML renders e with the shared email layout. Content is escaped;
// URLs other than http(s)/mailto are replaced by html/template.
func RenderHTML(e Email) string {
	var b bytes.Buffer
	if err := emailTmpl.Execute(&b, e); err != nil {
		return "" // the plain-text part is still sent
	}
	return b.String()
}
