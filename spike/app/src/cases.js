// Each case reproduces one error a founder's app commonly hits.
// Function names are real so we can see what minification does to them.
import { supabase } from "./supabase.js";

const users = {};

function currentUserId() {
  return "u_" + Math.floor(Math.random() * 1000);
}

function loadProfileName(user) {
  return user.profile.name; // TypeError: user is undefined
}

function showProfile() {
  return loadProfileName(users[currentUserId()]);
}

function submitCheckout() {
  const orderId = Date.now();
  throw new Error("Checkout failed for order " + orderId);
}

async function chargeCard() {
  await new Promise((r) => setTimeout(r, 10));
  throw new Error("Payment provider unavailable");
}

async function saveProfile() {
  // profiles.email is varchar(20): PostgREST 400, code 22001
  const email = "a.very.long.address." + Date.now() + "@example.com";
  await supabase.from("profiles").insert({ email });
}

async function saveSecret() {
  // RLS enabled with no policy: PostgREST 401, code 42501
  await supabase.from("secrets").insert({ note: "hello" });
}

async function signIn() {
  // GoTrue 400, error_code invalid_credentials
  await supabase.auth.signInWithPassword({ email: "nobody@example.com", password: "wrong-password" });
}

async function loadMissingTable() {
  // PostgREST 404, table not in schema cache
  await supabase.from("missing_table").select("*").eq("id", 5);
}

async function pingDeadService() {
  // Connection refused: fetch rejects, status 0
  try {
    await fetch("http://127.0.0.1:9/ping");
  } catch (e) {
    /* the add-on reports it; the app swallows it like many apps do */
  }
}

// Phase 4 cases (members table; see spike/fixcheck/db/schema.sql).

async function registerMember() {
  // members.email is unique: the second insert fails with 23505
  const member = { email: "dup@example.com", name: "Dana" };
  await supabase.from("members").insert(member);
  await supabase.from("members").insert(member);
}

async function addTeamMember() {
  // members.name is required: 23502
  await supabase.from("members").insert({ email: "new" + Date.now() + "@example.com" });
}

const route = { params: {} };

async function loadMember() {
  // the id isn't there yet: 22P02 invalid input syntax for type uuid: "undefined"
  await supabase.from("members").select("*").eq("id", route.params.memberId);
}

async function loadAccount() {
  // .single() with no matching row: PGRST116
  await supabase.from("members").select("*").eq("email", "nobody@example.com").single();
}

async function signUpWeak() {
  // GoTrue 422 weak_password
  await supabase.auth.signUp({ email: "weak" + Date.now() + "@example.com", password: "123" });
}

const state = { items: { first: "a" } };

function renderProjects() {
  // the API returned an object, not a list: "...items.map is not a function"
  return state.items.map((p) => p.name);
}

async function loadReport() {
  // no such endpoint: the SPA returns index.html and .json() throws a SyntaxError
  const res = await fetch("/api/report.json");
  return res.json();
}

export const cases = [
  { id: "js_typeerror", label: "Show profile (TypeError)", run: () => showProfile() },
  { id: "js_custom", label: "Checkout (thrown Error)", run: () => submitCheckout() },
  { id: "js_rejection", label: "Charge card (unhandled rejection)", run: () => { chargeCard(); } },
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
