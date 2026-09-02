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
//
// tailnetDomain is this node's own MagicDNS suffix, which is not under .ts.net
// when the tailnet uses a custom domain. The host's guard (internal/proxy,
// allowTailnetHost) must agree with this function on every name: the two are
// held together by testdata/tailnet-hosts.json, which both are tested against.
function tailtabIsTailnetHost(host, tailnetDomain) {
  if (!host) return false;
  var h = String(host).toLowerCase();
  if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);
  if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1);
  if (h === "") return false;

  // An IPv4-mapped IPv6 address is the IPv4 address it wraps: ::ffff:100.64.0.1
  // is 100.64.0.1. Unwrap it before deciding anything, which is what the host's
  // guard does with netip's Unmap; without this the two disagree.
  var mapped = h.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
  if (mapped) h = mapped[1];

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
    var d = String(tailnetDomain).replace(/^\.+|\.+$/g, "");
    // The suffix comes from the coordination server and is not trusted on
    // sight: a suffix of "com" would route the whole internet through the
    // tailnet. It has to look like a tailnet's own domain: lowercase, two or
    // more labels, each 1-63 characters of a-z, 0-9 and "-" and not starting
    // or ending with a dash, and not "ts.net", which is the public parent of
    // every tailnet and is covered by the rule above. The host's guard applies
    // the same test (internal/proxy, validMagicDNSSuffix).
    var ok = d.length > 0 && d.length <= 253 && d !== "ts.net" &&
      /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/.test(d);
    // On a label boundary only: with a suffix of "tail4d5e6f.example",
    // "evil-tail4d5e6f.example" is somebody else's name.
    if (ok && (h === d || (h.length > d.length && h.slice(-(d.length + 1)) === "." + d))) {
      return true;
    }
  }
  return false;
}

// tailtabBuildPac returns a PAC script for Chromium that applies exactly the
// rules above, sending tailnet hosts to the loopback proxy and everything else
// DIRECT.
//
// PROXY, not SOCKS5: the listener requires a credential, and Chromium has never
// implemented SOCKS5 authentication (research/browser.md section 1.3). Over PROXY the
// browser speaks HTTP to the listener, CONNECT for https and a forwarded request
// for http, so the 407 challenge reaches webRequest.onAuthRequired and the
// token can be supplied. Hostnames still travel unresolved, in the CONNECT
// line, which is what keeps MagicDNS resolution inside the node.
//
// There is deliberately no "; DIRECT" fallback: a tailnet request that cannot
// authenticate must fail, not quietly go out over the public internet.
function tailtabBuildPac(port, tailnetDomain) {
  var pac =
    tailtabIsTailnetHost.toString() +
    "\nfunction FindProxyForURL(url, host) {\n" +
    "  return tailtabIsTailnetHost(host, " +
    JSON.stringify(tailnetDomain || "") +
    ") ? " +
    JSON.stringify("PROXY 127.0.0.1:" + port) +
    " : \"DIRECT\";\n}\n";

  // Chromium refuses a PAC script containing any byte outside ASCII:
  // "'pacScript.data' supports only ASCII code (encode URLs in Punycode
  // format)". It refuses the whole script, so the browser is left with no
  // proxy configuration at all while the popup still says Connected, and
  // tailnet names quietly go out over the public internet.
  //
  // The source of tailtabIsTailnetHost is embedded above, comments and all, so
  // a single em dash in one of its comments is enough to cause that. Keep every
  // function whose source is embedded here in plain ASCII; this check is what
  // catches it if someone does not.
  if (!/^[\x00-\x7f]*$/.test(pac)) {
    throw new Error("tailtab: the PAC script has a non-ASCII character in it, which Chromium rejects");
  }
  return pac;
}

// Export for the popup and for tests; the background scripts use the globals.
if (typeof module !== "undefined" && module.exports) {
  module.exports = { tailtabIsTailnetHost: tailtabIsTailnetHost, tailtabBuildPac: tailtabBuildPac };
}
