## js_typeerror

Origin: js.null_property · severity medium

**The home page (/) crashes when `profile` isn't there yet**

On the home page (/), the page crashes with "Cannot read properties of undefined (reading 'profile')" and people can't continue.

```text
Please fix this problem in my app.

On the home page (/), the code reads `.profile` from a value that is sometimes undefined or null (the crash is near code named `run`), which crashes with "TypeError: Cannot read properties of undefined (reading 'profile')".

Find where `.profile` is read on the home page (/) and handle the empty case: wait until the data has loaded (show a loading state), and show an empty state or a message if it never arrives. Keep the change to that spot; don't rewrite the component.

Only make this one change.

To check it worked: Open the home page (/) on a slow connection, logged out, and with an account that has no data. It should never crash; you should see a loading or empty state.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## js_custom

Origin: agent (diagnose and fix) · severity medium

**On the home page (/), the page hits the error "Error: Checkout failed for order 1795463291742".**

On the home page (/), the page hits the error "Error: Checkout failed for order 1795463291742".

```text
Please fix this problem in my app.

Users of my app hit this error on the home page (/) (1 time). The app uses Supabase.

Example 1: Error: Checkout failed for order 1795463291742 (near code named `run`)

Find the cause in the code for the home page (/). First explain in one sentence what is wrong, then fix only that, with the smallest change that works. Don't rewrite components or change unrelated code.

Only make this one change.

To check it worked: Open the home page (/) and repeat what people were doing. The error should not appear in the browser console.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## js_rejection

Origin: agent (diagnose and fix) · severity medium

**On the home page (/), the page hits the error "Error: Payment provider unavailable".**

On the home page (/), the page hits the error "Error: Payment provider unavailable".

```text
Please fix this problem in my app.

Users of my app hit this error on the home page (/) (1 time). The app uses Supabase.

Example 1: Error: Payment provider unavailable

Find the cause in the code for the home page (/). First explain in one sentence what is wrong, then fix only that, with the smallest change that works. Don't rewrite components or change unrelated code.

Only make this one change.

To check it worked: Open the home page (/) and repeat what people were doing. The error should not appear in the browser console.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## net_22001

Origin: pg.value_too_long · severity medium

**Saving to profiles fails when a value is longer than 20 characters**

When someone submits data on the home page (/) that goes into the `profiles` table, saving fails because one value is longer than the database allows (20 characters). Nothing gets saved.

```text
Please fix this problem in my app.

In the Supabase table `profiles`, a text column is limited to 20 characters (type `character varying(20)`), and saving longer values fails with error 22001 "value too long for type character varying(20)". It happens when the app saves data on the home page (/).

Find the column in `profiles` whose type is `varchar(20)`, most likely the one the form on the home page (/) fills in (for example an email or name field), and change its type to `text` with a new migration. Don't change any other column or table.

Only make this one change.

To check it worked: On the home page (/), submit the form with a value longer than 20 characters (for example a 40-character email address). It should save without an error.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## net_42501

Origin: pg.rls_denied · severity high

**A database security rule blocks adding rows to secrets**

On the home page (/), the app tries to add data in the `secrets` table and Supabase refuses, so the action fails for the person using it.

```text
Please fix this problem in my app.

Supabase rejects adding rows to `secrets` with error 42501 "new row violates row-level security policy for table "secrets"" (HTTP 401). It happens on the home page (/) for a visitor who isn't logged in.

Add a row-level security policy on `secrets` that allows logged-in users to add only their own rows (for example where the row's `user_id` equals `auth.uid()`). If `secrets` has no column linking each row to its user, add `user_id uuid references auth.users default auth.uid()` in the same migration. If this action should only work for logged-in users, also make the home page (/) ask people to log in before they can do it.

Don't turn off row-level security and don't give anonymous users access to the whole table.

Only make this one change.

To check it worked: Log in, go to the home page (/) and repeat the action that failed: it should work. Then log out and try again: it should be refused with a clear message, not crash.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## net_auth

Origin: auth.invalid_credentials · severity low

**People typed a wrong email or password**

Log-in attempts on the home page (/) were rejected because the email or password was wrong.

```text
Please fix this problem in my app.

On the home page (/), when Supabase login returns "Invalid login credentials", show a clear message next to the form ("Wrong email or password") and a "Forgot password?" link that starts Supabase's password reset (`resetPasswordForEmail`). Don't change how sign-up or login work otherwise.

Only make this one change.

To check it worked: On the home page (/), log in with a wrong password. You should see the message and the reset link.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## net_404

Origin: pg.missing_table · severity high

**The app reads a table that doesn't exist: missing_table**

On the home page (/), loading data fails because the app asks for the `missing_table` table, which isn't in your database.

```text
Please fix this problem in my app.

On the home page (/), the app queries the Supabase table `missing_table`, which doesn't exist (error PGRST205: "Could not find the table 'public.missing_table' in the schema cache").

Check which tables exist in Supabase. If the code uses the wrong name, change it to the correct existing table. If `missing_table` is a new table the app needs, create it with a migration with the columns the code on the home page (/) uses. Don't rename existing tables.

Only make this one change.

To check it worked: Open the home page (/) and use the part that loads this data. It should load without errors.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## net_dead

Origin: net.local_address · severity high

**The app calls a local address (127.0.0.1:9) that users can't reach**

On the home page (/), a request to `127.0.0.1:9/ping` fails for everyone, because that address only exists on a developer's own computer.

```text
Please fix this problem in my app.

On the home page (/), the app calls `127.0.0.1:9/ping` (GET), which only works on a developer's computer, so it fails for every user ("Failed to fetch").

Replace that address with the real deployed service URL, read from an environment variable, and make the page show a friendly message if the request fails. Don't change what the request does.

Only make this one change.

To check it worked: Open the published app on the home page (/). The browser's network tab should show the request going to the real service and succeeding.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## pg_unique

Origin: pg.unique_violation · severity medium

**Saving to members fails because the email is already taken**

On the home page (/), saving fails when the `email` already exists in `members`, and the person isn't told why.

```text
Please fix this problem in my app.

On the home page (/), inserting into the Supabase table `members` fails with error 23505 (duplicate `email`).

Before saving, handle the case where a row with the same `email` already exists: either update that row instead (upsert on `email`) or show a clear message such as "This email is already registered". Don't remove the unique rule on `email`; it protects your data.

Only make this one change.

To check it worked: On the home page (/), save the same email twice. The second time you should see a clear message (or the existing row is updated), not an error.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## pg_not_null

Origin: pg.not_null · severity medium

**Saving to members fails because name is missing**

On the home page (/), saving fails because the app doesn't send a value for `name`, which `members` requires.

```text
Please fix this problem in my app.

On the home page (/), inserting into the Supabase table `members` fails with error 23502 because the required column `name` is empty.

Make the code on the home page (/) send a value for `name` (and make the form field required if the person types it in). Only if there is a sensible default for every row, give `name` a default value with a migration instead. Don't make `name` optional.

Only make this one change.

To check it worked: On the home page (/), fill in the form and save. It should save, and the new row in `members` should have `name` filled in.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## pg_invalid_uuid

Origin: pg.invalid_input · severity medium

**The app sends "undefined" where members expects a uuid**

On the home page (/), a request to `members` fails because a value isn't a valid uuid.

```text
Please fix this problem in my app.

On the home page (/), a query to the Supabase table `members` fails with error 22P02 "invalid input syntax for type uuid: "undefined"": the app sends `undefined` where a uuid is expected.

In the code on the home page (/) that queries `members`, don't run the query until that id is available (check it isn't undefined or empty first), and pass the real id.

Only make this one change.

To check it worked: Open the home page (/) fresh (reload it). The data should load without errors, including right after the page opens.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## pg_single

Origin: pg.single_row · severity medium

**home page (/) expects exactly one members row but finds none**

On the home page (/), loading from `members` fails when the expected row isn't there (or there are several).

```text
Please fix this problem in my app.

On the home page (/), the code loads one row from the Supabase table `members` with `.single()`, and it fails with PGRST116 when no matching row exists.

Change that query to `.maybeSingle()` and handle the case where nothing is found (show an empty state, or create the row if the app expects one to exist). Don't change the table.

Only make this one change.

To check it worked: Open the home page (/) with an account that has no `members` row yet. The page should show an empty state instead of an error.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## auth_weak_password

Origin: auth.weak_password · severity medium

**Sign-up fails because the password is too weak**

On the home page (/), sign-up is rejected because the password doesn't meet Supabase's password rules.

```text
Please fix this problem in my app.

On the home page (/), Supabase sign-up fails with `weak_password` ("Password should be at least 6 characters."). Show the password rules under the password field and check them before submitting (at least the minimum length Supabase requires), and show Supabase's message if it still rejects the password. Don't change the Supabase password settings.

Only make this one change.

To check it worked: On the home page (/), try to sign up with the password "123". You should see the rules before anything is sent.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## js_map

Origin: js.not_a_function · severity medium

**The home page (/) crashes: items.map is not a function**

On the home page (/), the page crashes with "hg.items.map is not a function".

```text
Please fix this problem in my app.

On the home page (/), the app crashes with "TypeError: hg.items.map is not a function": the data isn't a list when the code loops over it.

Find where `items.map` is used on the home page (/). Make sure the data is a list before using it (default to an empty list `[]` while loading, and check the API returns a list, not an object or an error). Keep the change to that spot.

Only make this one change.

To check it worked: Open the home page (/), including with an empty account. It should show the list (or an empty state) without crashing.

When you're done, tell me in one sentence what was wrong and what you changed.
```

## js_json

Origin: js.json_parse · severity medium

**The home page (/) expects JSON from /api/report.json but gets text/html**

On the home page (/), the request to `/api/report.json` returns text/html (HTTP 200) instead of JSON, so reading it crashes with "Unexpected token '<', "<!doctype "... is not valid JSON".

```text
Please fix this problem in my app.

On the home page (/), the app fetches `/api/report.json` and calls `.json()` on the response, but the response is text/html (HTTP 200), not JSON, so it crashes with "SyntaxError: Unexpected token '<', "<!doctype "... is not valid JSON".

Find that request. Check that `/api/report.json` is the right URL (fix it if it isn't), and before calling `.json()` check `response.ok` and that the content type is JSON; if not, show a friendly error instead of crashing. Don't change anything else.

Only make this one change.

To check it worked: Open the home page (/) with the network tab open. The request to /api/report.json should return JSON, and if it fails the page should show a message, not crash.

When you're done, tell me in one sentence what was wrong and what you changed.
```

