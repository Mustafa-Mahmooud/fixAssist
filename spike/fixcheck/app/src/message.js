// Tiny message area for the fixed copy (a real app already has UI for this).
export function showMessage(text, action) {
  window.dispatchEvent(new CustomEvent("app-message", { detail: { text, action } }));
}
