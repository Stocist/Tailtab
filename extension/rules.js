// Split-tunnel rules. This file is the single source of truth for what goes
// through the tailnet, in both browsers: Firefox calls tailtabIsTailnetHost
// from a proxy.onRequest listener, and Chromium runs the very same function
// inside a generated PAC script.
//
// Everything the tailnet cannot serve must stay DIRECT. That is what keeps
// ordinary browsing working when the native host dies (the failure mode of
// ts-browser-ext issue #18).

// tailtabIsTailnetHost reports whether host belongs to the tailnet.
//
// It must stay self-contained: its source is stringified into a PAC script,
// where nothing else in this file exists.
function tailtabIsTailnetHost(host, tailnetDomain) {
  if (!host) return false;
  var h = String(host).toLowerCase();
  if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);
  if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1);
  if (h === "") return false;

  // Never proxy the loopback: the proxy itself lives there.
  if (h === "localhost" || h === "::1" || h.indexOf("127.") === 0) return false;
  if (h.length > 10 && h.slice(-10) === ".localhost") return false;

  // Tailscale's CGNAT range, 100.64.0.0/10.
  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    var a = parseInt(v4[1], 10);
    var b = parseInt(v4[2], 10);
    return a === 100 && b >= 64 && b <= 127;
  }

  // Tailscale's ULA range, fd7a:115c:a1e0::/48.
  if (h.indexOf(":") !== -1) {
    return h.indexOf("fd7a:115c:a1e0") === 0;
  }

  // A bare number is never a MagicDNS name: it is an obfuscated form of an
  // IPv4 address, such as 2130706433 or 0x7f000001 for 127.0.0.1.
  if (/^\d+$/.test(h) || /^0x[0-9a-f]+$/.test(h)) return false;

  // A single-label name is a MagicDNS short name, e.g. "wiki". A machine on
  // the local network would also match, which is the trade phase 0 accepts.
  if (h.indexOf(".") === -1) return true;

  // Any tailnet's MagicDNS suffix, plus this node's own, which may be a
  // custom domain rather than *.ts.net.
  if (h.length > 7 && h.slice(-7) === ".ts.net") return true;
  if (tailnetDomain) {
    var d = String(tailnetDomain).toLowerCase().replace(/^\.+|\.+$/g, "");
    if (d && (h === d || (h.length > d.length && h.slice(-(d.length + 1)) === "." + d))) {
      return true;
    }
  }
  return false;
}

// tailtabBuildPac returns a PAC script for Chromium that applies exactly the
// rules above, sending tailnet hosts to the loopback SOCKS5 proxy and
// everything else DIRECT.
function tailtabBuildPac(port, tailnetDomain) {
  return (
    tailtabIsTailnetHost.toString() +
    "\nfunction FindProxyForURL(url, host) {\n" +
    "  return tailtabIsTailnetHost(host, " +
    JSON.stringify(tailnetDomain || "") +
    ") ? " +
    JSON.stringify("SOCKS5 127.0.0.1:" + port) +
    " : \"DIRECT\";\n}\n"
  );
}

// Export for the popup and for tests; the background scripts use the globals.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { tailtabIsTailnetHost: tailtabIsTailnetHost, tailtabBuildPac: tailtabBuildPac };
}
