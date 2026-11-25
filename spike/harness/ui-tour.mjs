// Drives the dashboard like a founder and saves screenshots for review.
//
//   node harness/ui-tour.mjs signup    sign up via magic link (dev mailer), create
//                                      the project, print its public key
//   node harness/ui-tour.mjs screens   screenshot setup, issues and an issue
//
// Env: APP_URL (default http://127.0.0.1:8787), MAIL_DIR (the server's
// EMAIL_DEV_DIR), OUT_DIR (screenshots), TOUR_EMAIL.
import { readdirSync, readFileSync, mkdirSync, existsSync } from "node:fs";
import { join } from "node:path";
import { chromium } from "playwright";

const APP = process.env.APP_URL || "http://127.0.0.1:8787";
const MAIL_DIR = process.env.MAIL_DIR || "tmp/mail";
const OUT = process.env.OUT_DIR || "tmp/screens";
const EMAIL = process.env.TOUR_EMAIL || "founder@example.com";
const STATE = join(OUT, "state.json");
const PROJECT = "Spike app";
mkdirSync(OUT, { recursive: true });

function latestLink() {
  const files = readdirSync(MAIL_DIR).filter((f) => f.endsWith(".eml")).sort();
  const text = readFileSync(join(MAIL_DIR, files[files.length - 1]), "utf8");
  const m = text.match(/https?:\/\/\S+\/auth\/magic\?token=[A-Za-z0-9_-]+/);
  if (!m) throw new Error("no magic link in the latest email");
  return m[0];
}

async function signup(browser) {
  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(`${APP}/login`);
  await page.screenshot({ path: join(OUT, "01-login.png") });
  await page.fill("#email", EMAIL);
  await page.click("button[type=submit]");
  await page.waitForSelector("text=Check your email");
  await page.goto(latestLink());
  await page.click("button[type=submit]");
  await page.waitForURL(/\/projects$/);
  if (!(await page.locator(`.project-card a.name:has-text("${PROJECT}")`).count())) {
    await page.screenshot({ path: join(OUT, "02-projects-empty.png"), fullPage: true });
    await page.fill("#name", PROJECT);
    await page.selectOption("#agent", "lovable");
    await page.selectOption("#backend", "supabase");
    await page.fill("#domains", "localhost");
    await page.click("form[action='/projects'] button[type=submit]");
  } else {
    await page.click(`.project-card:has-text("${PROJECT}") a:has-text("Setup")`);
  }
  await page.waitForURL(/\/setup$/);
  await page.screenshot({ path: join(OUT, "03-setup-waiting.png"), fullPage: true });
  const tag = await page.locator("#script-tag").textContent();
  const key = tag.match(/sdk\/([0-9a-f]+)\.js/)[1];
  await ctx.storageState({ path: STATE });
  console.log(JSON.stringify({ key, setup: page.url() }));
}

async function screens(browser) {
  if (!existsSync(STATE)) throw new Error("run signup first");
  const ctx = await browser.newContext({ storageState: STATE });
  const page = await ctx.newPage();
  await page.goto(`${APP}/projects`);
  await page.screenshot({ path: join(OUT, "04-projects.png"), fullPage: true });
  await page.click(`.project-card:has-text("${PROJECT}") a:has-text("Setup")`);
  await page.waitForSelector("text=Connected.");
  await page.screenshot({ path: join(OUT, "05-setup-connected.png"), fullPage: true });
  await page.click(".tabs a:has-text('Issues')");
  await page.waitForSelector("#issues");
  await page.screenshot({ path: join(OUT, "06-issues.png"), fullPage: true });
  await page.click("#issues a.title:has-text(\"profiles\") >> nth=0");
  await page.waitForSelector("text=Technical details");
  if (await page.locator("button:has-text(\"Explain now\")").count()) {
    await page.click("button:has-text(\"Explain now\")");
    await page.waitForSelector("text=Fix it with");
  }
  await page.screenshot({ path: join(OUT, "07-issue.png"), fullPage: true });
  await page.click(".tabs a:has-text(\"Cursor\")");
  await page.waitForSelector("text=Fix it with Cursor");
  await page.screenshot({ path: join(OUT, "07b-issue-cursor.png"), fullPage: false });
  await page.click(".tabs a:has-text(\"Alerts\")");
  await page.screenshot({ path: join(OUT, "09-alerts.png"), fullPage: true });

  const phone = await browser.newContext({ storageState: STATE, viewport: { width: 390, height: 844 }, colorScheme: "dark" });
  const p2 = await phone.newPage();
  await p2.goto(page.url().replace(/\/issues\/\d+$/, "/issues"));
  await p2.screenshot({ path: join(OUT, "08-issues-phone-dark.png"), fullPage: true });
  const overflow = await p2.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  console.log(JSON.stringify({ screenshots: OUT, phoneHorizontalOverflow: overflow }));
}

// Phase 6 screens: overview, pages, funnels (creates the suggested funnel
// if there is none yet) and insights.
async function analyticsScreens(browser) {
  if (!existsSync(STATE)) throw new Error("run signup first");
  const ctx = await browser.newContext({ storageState: STATE });
  const page = await ctx.newPage();
  await page.goto(`${APP}/projects`);
  await page.click(`.project-card:has-text("${PROJECT}") a.name`);
  const base = page.url().replace(/\/issues.*$/, "");
  await page.goto(`${base}/funnels`);
  if (await page.locator("button:has-text('Create this funnel')").count()) {
    await page.click("button:has-text('Create this funnel')");
    await page.waitForURL(/\/funnels$/);
  }
  await page.screenshot({ path: join(OUT, "10-funnels.png"), fullPage: true });
  await page.goto(`${base}/overview`);
  await page.screenshot({ path: join(OUT, "11-overview.png"), fullPage: true });
  await page.goto(`${base}/pages`);
  await page.screenshot({ path: join(OUT, "12-pages.png"), fullPage: true });
  await page.goto(`${base}/insights`);
  await page.click("button:has-text('Find insights now')");
  await page.waitForURL(/\/insights$/);
  await page.screenshot({ path: join(OUT, "13-insights.png"), fullPage: true });
  const phone = await browser.newContext({ storageState: STATE, viewport: { width: 390, height: 844 }, colorScheme: "dark" });
  const p2 = await phone.newPage();
  await p2.goto(`${base}/funnels`);
  await p2.screenshot({ path: join(OUT, "14-funnels-phone-dark.png"), fullPage: true });
  const overflow = await p2.evaluate(() => document.documentElement.scrollWidth > window.innerWidth);
  console.log(JSON.stringify({ screenshots: OUT, phoneHorizontalOverflow: overflow }));
}

const browser = await chromium.launch();
try {
  if (process.argv[2] === "signup") await signup(browser);
  else if (process.argv[2] === "screens") await screens(browser);
  else if (process.argv[2] === "analytics") await analyticsScreens(browser);
  else throw new Error("usage: ui-tour.mjs signup|screens|analytics");
} finally {
  await browser.close();
}
