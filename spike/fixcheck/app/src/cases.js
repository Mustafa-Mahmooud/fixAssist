// FIXED COPY for the Phase 4 fix check. Each change follows the generated
// prompt in ../prompts.md for that case, and nothing else. Diff against
// spike/app/src/cases.js to see exactly what an agent changed.
import { supabase } from "./supabase.js";
import { showMessage } from "./message.js";

const users = {};

function currentUserId() {
  return "u_" + Math.floor(Math.random() * 1000);
}

function loadProfileName(user) {
  // js_typeerror: the user may not be loaded (or exist) yet; show an empty state.
  if (!user || !user.profile) {
    showMessage("No profile yet.");
    return null;
  }
  return user.profile.name;
}

function showProfile() {
  return loadProfileName(users[currentUserId()]);
}

function submitCheckout() {
  // js_custom: submitCheckout always threw instead of completing the checkout;
  // it now returns the new order id.
  const orderId = Date.now();
  return orderId;
}

async function chargeCard() {
  await new Promise((r) => setTimeout(r, 10));
  throw new Error("Payment provider unavailable");
}

async function saveProfile() {
  // net_22001: fixed in the database (fixes/001_profiles_email_text.sql).
  const email = "a.very.long.address." + Date.now() + "@example.com";
  await supabase.from("profiles").insert({ email });
}

async function saveSecret() {
  // net_42501: only logged-in users may add secrets (policy in
  // fixes/002_secrets_policy.sql); ask visitors to log in first.
  const { data } = await supabase.auth.getSession();
  if (!data.session) {
    showMessage("Please log in to save this.");
    return;
  }
  await supabase.from("secrets").insert({ note: "hello" });
}

async function signIn() {
  // net_auth: show a clear message and a reset link on wrong credentials.
  const { error } = await supabase.auth.signInWithPassword({ email: "nobody@example.com", password: "wrong-password" });
  if (error && error.message === "Invalid login credentials") {
    showMessage("Wrong email or password. Forgot password? Use the reset link.", {
      label: "Forgot password?",
      onClick: () => supabase.auth.resetPasswordForEmail("nobody@example.com"),
    });
  }
}

async function loadMissingTable() {
  // net_404: `missing_table` doesn't exist; the data lives in `profiles`.
  await supabase.from("profiles").select("*").eq("id", 5);
}

async function pingDeadService() {
  // net_dead: the service URL comes from the environment, not 127.0.0.1.
  try {
    await fetch(import.meta.env.VITE_PING_URL);
  } catch (e) {
    showMessage("We couldn't reach the service. Please try again.");
  }
}

// Phase 4 cases (members table; see spike/fixcheck/db/schema.sql).

async function registerMember() {
  // pg_unique: upsert on email instead of inserting a duplicate.
  const member = { email: "dup@example.com", name: "Dana" };
  await supabase.from("members").upsert(member, { onConflict: "email" });
  await supabase.from("members").upsert(member, { onConflict: "email" });
}

async function addTeamMember() {
  // pg_not_null: send the required name.
  await supabase.from("members").insert({ email: "new" + Date.now() + "@example.com", name: "New member" });
}

const route = { params: {} };

async function loadMember() {
  // pg_invalid_uuid: don't query until the id is known.
  if (!route.params.memberId) return;
  await supabase.from("members").select("*").eq("id", route.params.memberId);
}

async function loadAccount() {
  // pg_single: maybeSingle + empty state when there's no row.
  const { data } = await supabase.from("members").select("*").eq("email", "nobody@example.com").maybeSingle();
  if (!data) showMessage("No account details yet.");
}

const MIN_PASSWORD = 6;

async function signUpWeak() {
  // auth_weak_password: check the rules before sending; show Supabase's message if it still fails.
  const password = "123";
  if (password.length < MIN_PASSWORD) {
    showMessage(`Password must be at least ${MIN_PASSWORD} characters.`);
    return;
  }
  const { error } = await supabase.auth.signUp({ email: "weak" + Date.now() + "@example.com", password });
  if (error) showMessage(error.message);
}

const state = { items: { first: "a" } };

function renderProjects() {
  // js_map: make sure the data is a list before using it.
  const items = Array.isArray(state.items) ? state.items : [];
  return items.map((p) => p.name);
}

async function loadReport() {
  // js_json: check the response before reading it as JSON; show a message otherwise.
  const res = await fetch("/api/report.json");
  const type = res.headers.get("content-type") || "";
  if (!res.ok || !type.includes("application/json")) {
    showMessage("The report couldn't be loaded. Please try again later.");
    return null;
  }
  return res.json();
}

export const cases = [
  { id: "js_typeerror", label: "Show profile (TypeError)", run: () => showProfile() },
  { id: "js_custom", label: "Checkout (thrown Error)", run: () => submitCheckout() },
  {
    id: "js_rejection",
    label: "Charge card (unhandled rejection)",
    // js_rejection: the payment call can fail; handle it instead of leaving the rejection unhandled.
    run: () => { chargeCard().catch(() => showMessage("Payment couldn't be processed. Please try again.")); },
  },
  { id: "net_22001", label: "Save profile (value too long)", run: () => { saveProfile(); } },
  { id: "net_42501", label: "Save secret (RLS)", run: () => { saveSecret(); } },
  { id: "net_auth", label: "Sign in (bad password)", run: () => { signIn(); } },
  { id: "net_404", label: "Load missing table", run: () => { loadMissingTable(); } },
  { id: "net_dead", label: "Ping dead service", run: () => { pingDeadService(); } },
  { id: "pg_unique", label: "Register member twice (duplicate email)", run: () => { registerMember(); } },
  { id: "pg_not_null", label: "Add team member without a name", run: () => { addTeamMember(); } },
  { id: "pg_invalid_uuid", label: "Load member before id is known", run: () => { loadMember(); } },
  { id: "pg_single", label: "Load account (no row)", run: () => { loadAccount(); } },
  { id: "auth_weak_password", label: "Sign up with password 123", run: () => { signUpWeak(); } },
  { id: "js_map", label: "Render projects (object, not list)", run: () => renderProjects() },
  { id: "js_json", label: "Load report (not JSON)", run: () => { loadReport(); } },
];
