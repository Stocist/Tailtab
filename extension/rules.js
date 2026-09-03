// Split-tunnel rules. This file is the single source of truth for what goes
// through the tailnet, in both browsers: Firefox calls tailtabIsTailnetHost
// from a proxy.onRequest listener, and Chromium runs the very same function
// inside a generated PAC script.
//
// Everything the tailnet cannot serve must stay DIRECT. That is what keeps
// ordinary browsing working when the native host dies (the failure mode of
// ts-browser-ext issue #18).

// tailtabInRoutes reports whether h, an IP address literal already lower-cased
// and unbracketed, falls inside any of routes, an array of CIDR strings such as
// "192.168.1.0/24" or "fd00:1:2::/64". Subnet routes are what peers advertise
// and the admin console approves; an address inside one reaches the tailnet
// through that peer, so it is a tailnet destination in both modes.
//
// Self-contained and pure ASCII, like the two rules that call it: its source is
// embedded in the PAC script. A malformed route is ignored, never widened.
function tailtabInRoutes(h, routes) {
  if (!routes || !routes.length) return false;
  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  function ipv4(m) {
    var n = 0;
    for (var i = 1; i <= 4; i++) {
      var o = parseInt(m[i], 10);
      if (o > 255) return -1;
      n = n * 256 + o;
    }
    return n;
  }
  function ipv6(str) {
    // Returns eight 16-bit numbers, or null.
    if (str.indexOf(".") !== -1) return null; // mixed notation is not used here
    var halves = str.split("::");
    if (halves.length > 2) return null;
    var head = halves[0] ? halves[0].split(":") : [];
    var tail = halves.length === 2 && halves[1] ? halves[1].split(":") : [];
    if (halves.length === 1 && head.length !== 8) return null;
    if (halves.length === 2 && head.length + tail.length > 7) return null;
    var out = [];
    var groups = head.concat([]);
    var fill = halves.length === 2 ? 8 - head.length - tail.length : 0;
    for (var f = 0; f < fill; f++) groups.push("0");
    groups = groups.concat(tail);
    for (var g = 0; g < groups.length; g++) {
      if (!/^[0-9a-f]{1,4}$/.test(groups[g])) return null;
      out.push(parseInt(groups[g], 16));
    }
    return out.length === 8 ? out : null;
  }
  if (v4) {
    var ip = ipv4(v4);
    if (ip < 0) return false;
    for (var i = 0; i < routes.length; i++) {
      var r = String(routes[i]).split("/");
      if (r.length !== 2) continue;
      var rm = r[0].match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
      var bits = parseInt(r[1], 10);
      if (!rm || !(bits >= 0 && bits <= 32) || String(bits) !== r[1]) continue;
      var net = ipv4(rm);
      if (net < 0) continue;
      if (bits === 0) return true;
      // Compare the top "bits" bits. Division keeps this in plain arithmetic:
      // JavaScript's bitwise operators are 32-bit signed and a /0 route would
      // shift by 32, which is a no-op.
      var scale = Math.pow(2, 32 - bits);
      if (Math.floor(ip / scale) === Math.floor(net / scale)) return true;
    }
    return false;
  }
  if (h.indexOf(":") !== -1) {
    var a6 = ipv6(h);
    if (!a6) return false;
    for (var j = 0; j < routes.length; j++) {
      var r6 = String(routes[j]).toLowerCase().split("/");
      if (r6.length !== 2) continue;
      var bits6 = parseInt(r6[1], 10);
      if (!(bits6 >= 0 && bits6 <= 128) || String(bits6) !== r6[1]) continue;
      var n6 = ipv6(r6[0]);
      if (!n6) continue;
      var ok = true;
      for (var k = 0; k < 8 && ok; k++) {
        var take = bits6 - 16 * k;
        if (take <= 0) break;
        if (take >= 16) {
          ok = a6[k] === n6[k];
        } else {
          var sc = Math.pow(2, 16 - take);
          ok = Math.floor(a6[k] / sc) === Math.floor(n6[k] / sc);
        }
      }
      if (ok) return true;
    }
    return false;
  }
  return false;
}

// tailtabIsTailnetHost reports whether host belongs to the tailnet.
//
// It must stay self-contained: its source is stringified into a PAC script,
// where nothing else in this file exists.
//
// tailnetDomain is this node's own MagicDNS suffix, which is not under .ts.net
// when the tailnet uses a custom domain. The host's guard (internal/proxy,
// allowTailnetHost) must agree with this function on every name: the two are
// held together by testdata/tailnet-hosts.json, which both are tested against.
function tailtabIsTailnetHost(host, tailnetDomain, routes) {
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
    if (a === 100 && b >= 64 && b <= 127) return true;
    // An address inside a subnet a peer routes for the tailnet.
    return tailtabInRoutes(h, routes);
  }

  // Tailscale's ULA range, fd7a:115c:a1e0::/48, or a routed IPv6 subnet.
  if (h.indexOf(":") !== -1) {
    if (h.indexOf("fd7a:115c:a1e0") === 0) return true;
    return tailtabInRoutes(h, routes);
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

// tailtabExitModeProxies reports whether host goes through the proxy while an
// exit node is carrying this profile's traffic.
//
// Exit mode inverts the split tunnel: the point of an exit node is that
// everything leaves through it, so the public internet is proxied and only what
// belongs to a local network is left alone. The addresses refused here are the
// ones that would otherwise be dialled on the exit node's LAN rather than the
// user's own, plus loopback, which is this machine.
//
// The host applies the same rule in Go (internal/proxy, allowExitHost), and the
// two are held together by testdata/exit-mode-hosts.json.
//
// Like tailtabIsTailnetHost, this must stay self-contained and pure ASCII: its
// source is stringified into a PAC script.
function tailtabExitModeProxies(host, routes) {
  if (!host) return false;
  var h = String(host).toLowerCase();
  if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);
  if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1);
  if (h === "") return false;

  var mapped = h.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
  if (mapped) h = mapped[1];

  if (h === "localhost") return false;
  if (h.length > 10 && h.slice(-10) === ".localhost") return false;

  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    var a = parseInt(v4[1], 10);
    var b = parseInt(v4[2], 10);
    if (a === 100 && b >= 64 && b <= 127) return true; // the tailnet itself
    // A routed subnet is still the tailnet's in exit mode: tailscaled sends it
    // to the subnet router, not the exit node.
    if (tailtabInRoutes(h, routes)) return true;
    if (a === 0 || a === 10 || a === 127) return false; // this host, private, loopback
    if (a === 172 && b >= 16 && b <= 31) return false; // 172.16/12
    if (a === 192 && b === 168) return false; // 192.168/16
    if (a === 169 && b === 254) return false; // link-local
    if (a >= 224) return false; // multicast, reserved, broadcast
    return true;
  }

  if (h.indexOf(":") !== -1) {
    if (h.indexOf("fd7a:115c:a1e0") === 0) return true; // the tailnet's ULA
    if (tailtabInRoutes(h, routes)) return true; // a routed IPv6 subnet
    if (h === "::1" || h === "::") return false;
    // fe80::/10 is fe80 through febf; fc00::/7 is every fc and fd; ff00::/8 is
    // multicast.
    if (h.indexOf("fe8") === 0 || h.indexOf("fe9") === 0) return false;
    if (h.indexOf("fea") === 0 || h.indexOf("feb") === 0) return false;
    if (h.indexOf("fc") === 0 || h.indexOf("fd") === 0) return false;
    if (h.indexOf("ff") === 0) return false;
    return true;
  }

  // A bare number is an obfuscated IPv4 address, never a name.
  if (/^\d+$/.test(h) || /^0x[0-9a-f]+$/.test(h)) return false;

  // Everything else is a name: a MagicDNS short name, or a public one, and in
  // exit mode both go to the proxy.
  return true;
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
function tailtabBuildPac(port, tailnetDomain, exitMode, routes) {
  var target = JSON.stringify("PROXY 127.0.0.1:" + port);
  // Only well-formed CIDR strings reach the script; anything else is dropped
  // here as well as ignored inside the rule.
  var clean = [];
  var list = Array.isArray(routes) ? routes : [];
  for (var i = 0; i < list.length; i++) {
    if (/^[0-9a-fA-F:.]+\/\d{1,3}$/.test(String(list[i]))) clean.push(String(list[i]).toLowerCase());
  }
  var routesJSON = JSON.stringify(clean);
  var pac;
  if (exitMode) {
    // An exit node is selected, so everything that is not local goes through
    // it. The browser and the host switch on the same status field, or one
    // sends traffic the other refuses.
    pac =
      tailtabInRoutes.toString() +
      "\n" +
      tailtabExitModeProxies.toString() +
      "\nvar TAILTAB_ROUTES = " + routesJSON + ";\n" +
      "function FindProxyForURL(url, host) {\n" +
      "  return tailtabExitModeProxies(host, TAILTAB_ROUTES) ? " +
      target +
      " : \"DIRECT\";\n}\n";
  } else {
    pac =
      tailtabInRoutes.toString() +
      "\n" +
      tailtabIsTailnetHost.toString() +
      "\nvar TAILTAB_ROUTES = " + routesJSON + ";\n" +
      "function FindProxyForURL(url, host) {\n" +
      "  return tailtabIsTailnetHost(host, " +
      JSON.stringify(tailnetDomain || "") +
      ", TAILTAB_ROUTES) ? " +
      target +
      " : \"DIRECT\";\n}\n";
  }

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
  module.exports = {
    tailtabInRoutes: tailtabInRoutes,
    tailtabIsTailnetHost: tailtabIsTailnetHost,
    tailtabExitModeProxies: tailtabExitModeProxies,
    tailtabBuildPac: tailtabBuildPac,
  };
}
