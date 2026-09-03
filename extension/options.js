// Tailtab settings page. Everything here is a value the background script
// reads when it needs it; nothing talks to the host directly.

"use strict";

const api = typeof browser !== "undefined" ? browser : typeof chrome !== "undefined" ? chrome : null;
const el = (id) => document.getElementById(id);

// tailtabValidControlURL mirrors the host's check (nm.ValidControlURL): http or
// https, a host, no credentials, no query or fragment. The host checks again;
// this just gives the user the error before they leave the page.
function tailtabValidControlURL(s) {
  if (!s) return "";
  if (s.length > 512) return "That URL is too long.";
  let u;
  try {
    u = new URL(s);
  } catch (e) {
    return "That is not a URL. Include the scheme, e.g. https://headscale.example.com";
  }
  if (u.protocol !== "http:" && u.protocol !== "https:") return "The server must be http:// or https://.";
  if (!u.hostname) return "The URL has no host.";
  if (u.username || u.password) return "Do not put credentials in the URL.";
  if (u.search || u.hash) return "The URL must not have a query string or fragment.";
  return "";
}

function show(id, text) {
  const node = el(id);
  node.hidden = !text;
  node.textContent = text || "";
}

function load() {
  return api.storage.local.get("controlURL").then((v) => {
    el("controlURL").value = (v && v.controlURL) || "";
  });
}

function save(value) {
  const trimmed = String(value || "").trim().replace(/\/+$/, "");
  const problem = tailtabValidControlURL(trimmed);
  show("error", problem);
  show("saved", "");
  if (problem) return Promise.resolve(false);
  return api.storage.local.set({ controlURL: trimmed }).then(() => {
    el("controlURL").value = trimmed;
    show("saved", trimmed ? "Saved. Used for the next login." : "Saved. Using Tailscale's server.");
    return true;
  });
}

// Wired only where there is a page; the validator above is also required by
// the test suite under node.
if (typeof document !== "undefined" && api) {
  el("save").addEventListener("click", () => save(el("controlURL").value));
  el("reset").addEventListener("click", () => save(""));
  el("controlURL").addEventListener("keydown", (e) => {
    if (e.key === "Enter") save(el("controlURL").value);
  });
  load();
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = { tailtabValidControlURL: tailtabValidControlURL };
}
