package web

import "strings"

// Server-side errors: the app's backend reports its own exceptions with the
// official Sentry SDK for its language, pointed at this project's DSN. The
// ingest endpoint speaks the Sentry protocol, so no custom SDK is needed.

// ServerSnippet is the setup for one language or framework.
type ServerSnippet struct {
	Name string
	Code string
}

// ServerSnippets returns copy-paste setups for the common backends. Each one
// sends errors only (no performance data) and no personal data.
func ServerSnippets(dsn string) []ServerSnippet {
	list := []ServerSnippet{
		{"Node.js (Express, Nest, Next.js API, ...)", `npm install @sentry/node

// instrument.js — load it before anything else:
//   node --require ./instrument.js server.js
const Sentry = require("@sentry/node");
Sentry.init({ dsn: "DSN", sendDefaultPii: false, tracesSampleRate: 0 });

// Express: after all your routes
Sentry.setupExpressErrorHandler(app);`},
		{"Python (Django, Flask, FastAPI, ...)", `pip install sentry-sdk

# At startup (settings.py, app.py or main.py), before the app is created:
import sentry_sdk
sentry_sdk.init(dsn="DSN", send_default_pii=False, traces_sample_rate=0)`},
		{".NET (ASP.NET Core)", `dotnet add package Sentry.AspNetCore

// Program.cs, after "var builder = WebApplication.CreateBuilder(args);"
builder.WebHost.UseSentry(o =>
{
    o.Dsn = "DSN";
    o.SendDefaultPii = false;
    o.TracesSampleRate = 0;
});`},
		{"PHP (Laravel or plain PHP)", `# Laravel
composer require sentry/sentry-laravel
php artisan sentry:publish --dsn=DSN
# Laravel 11+: in bootstrap/app.php
#   ->withExceptions(function (Exceptions $exceptions) {
#       \Sentry\Laravel\Integration::handles($exceptions);
#   })

# Plain PHP
composer require sentry/sentry
\Sentry\init(['dsn' => 'DSN', 'send_default_pii' => false]);`},
		{"Ruby (Rails)", `# Gemfile
gem "sentry-ruby"
gem "sentry-rails"

# config/initializers/sentry.rb
Sentry.init do |config|
  config.dsn = "DSN"
  config.send_default_pii = false
end`},
		{"Java (Spring Boot)", `// build.gradle (or the same artifact in pom.xml)
implementation "io.sentry:sentry-spring-boot-starter-jakarta:<latest version>"

# application.properties
sentry.dsn=DSN
sentry.send-default-pii=false`},
		{"Go (net/http, Gin, Echo, ...)", `go get github.com/getsentry/sentry-go

// main()
sentry.Init(sentry.ClientOptions{Dsn: "DSN"})
defer sentry.Flush(2 * time.Second)

// net/http: catch panics in handlers
handler = sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle(handler)
// errors you handle yourself:
sentry.CaptureException(err)`},
	}
	for i := range list {
		list[i].Code = strings.ReplaceAll(list[i].Code, "DSN", dsn)
	}
	return list
}

// ServerPrompt is pasted into the founder's AI agent (or sent to their
// developer). It works for any backend language.
func ServerPrompt(dsn string) string {
	return "Add server-side error reporting to this app's backend.\n\n" +
		"Use the official Sentry SDK for the backend's language and framework (Node.js, Python, .NET, PHP, Ruby, Java, Go, ...), " +
		"set up at application startup so that unhandled errors in web requests and background jobs are reported. Use this DSN:\n\n" +
		dsn + "\n\n" +
		"Settings: send errors only (no performance tracing: traces sample rate 0), don't send personal data (send default PII off), " +
		"and environment \"production\". If the hosting provides a value that changes on every deploy (such as the git commit), " +
		"pass it as the release (most SDKs read the SENTRY_RELEASE environment variable).\n\n" +
		"Don't change anything else in the app. Don't add the browser SDK: the website already has its own script.\n" +
		"To check: deploy, make one request fail on purpose (for example a temporary test route that throws an error), " +
		"confirm it appears in Fix Assistant under Issues as a server error, then remove the test route."
}
