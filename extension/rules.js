// Gecko calls these routing predicates directly; Chromium embeds them in PAC.
// In split mode, public traffic stays DIRECT when the native host fails.

// PAC embeds this function, so keep it self-contained and ASCII. Malformed
// routes are ignored, never widened, in parity with the host parser.
function tailtabInRoutes(h, routes) {
  if (!routes || !routes.length) return false;
  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  function ipv4(m) {
    var n = 0;
    for (var i = 1; i <= 4; i++) {
      // Match the host parser: reject leading zeros instead of guessing a base.
      if (m[i].length > 1 && m[i].charAt(0) === "0") return -1;
      var o = parseInt(m[i], 10);
      if (o > 255) return -1;
      n = n * 256 + o;
    }
    return n;
  }
  // Match node.UsableRoute: reject broad routes and overlaps with local,
  // link-local, multicast, unspecified, or reserved ranges.
  var reserved4 = [[0, 8], [2130706432, 8], [2851995648, 16], [3758096384, 4], [4026531840, 4]];
  function v4Overlaps(net, bits) {
    for (var q = 0; q < reserved4.length; q++) {
      var rb = Math.min(bits, reserved4[q][1]);
      var sc = Math.pow(2, 32 - rb);
      if (Math.floor(net / sc) === Math.floor(reserved4[q][0] / sc)) return true;
    }
    return false;
  }
  function v6Overlaps(n6) {
    // With the /16 floor, the top group identifies link-local and multicast.
    var zeros = true;
    for (var z = 0; z < 7; z++) if (n6[z] !== 0) { zeros = false; break; }
    if (zeros && n6[7] <= 1) return true;
    if (n6[0] >= 0xfe80 && n6[0] <= 0xfebf) return true;
    if (Math.floor(n6[0] / 256) === 0xff) return true;
    return false;
  }
  function ipv6(str) {
    if (str.indexOf(".") !== -1) return null;
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
      if (bits < 8) continue;
      // Avoid signed 32-bit JavaScript bitwise operations.
      var scale = Math.pow(2, 32 - bits);
      var netMasked = Math.floor(net / scale) * scale;
      if (v4Overlaps(netMasked, bits)) continue;
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
      if (bits6 < 16) continue;
      if (v6Overlaps(n6)) continue;
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

// PAC stringifies this function, so keep it self-contained. Its parsing must
// match allowTailnetHost, as enforced by testdata/tailnet-hosts.json.
function tailtabIsTailnetHost(host, tailnetDomain, routes) {
  if (!host) return false;
  var h = String(host).toLowerCase();
  if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);
  if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1);
  if (h === "") return false;
  if (h.indexOf("%") !== -1) return false; // a zoned address, refused by hostAddr too

  // Match netip.Unmap before applying IPv4 rules.
  var mapped = h.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
  if (mapped) h = mapped[1];

  // Never proxy the loopback: the proxy itself lives there.
  if (h === "localhost" || h === "::1") return false;
  if (h.length > 10 && h.slice(-10) === ".localhost") return false;

  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    // Match the host parser by rejecting leading-zero IPv4 octets.
    for (var oi = 1; oi <= 4; oi++) if (v4[oi].length > 1 && v4[oi].charAt(0) === "0") return false;
    var a = parseInt(v4[1], 10);
    var b = parseInt(v4[2], 10);
    if (a === 100 && b >= 64 && b <= 127) return true;
    if (a === 0 || a === 127 || (a === 169 && b === 254) || a >= 224) return false;
    return tailtabInRoutes(h, routes);
  }

  if (h.indexOf(":") !== -1) {
    if (h.indexOf("fd7a:115c:a1e0") === 0) return true;
    return tailtabInRoutes(h, routes);
  }

  // Reject numeric IPv4 obfuscations rather than treating them as MagicDNS.
  if (/^\d+$/.test(h) || /^0x[0-9a-f]+$/.test(h)) return false;

  // Single-label names are MagicDNS; this intentionally includes local names.
  if (h.indexOf(".") === -1) return true;

  if (h.length > 7 && h.slice(-7) === ".ts.net") return true;
  if (tailnetDomain) {
    var d = String(tailnetDomain).replace(/^\.+|\.+$/g, "");
    // Treat the coordination-server suffix as untrusted. Match
    // validMagicDNSSuffix so a broad value cannot proxy the public internet.
    var ok = d.length > 0 && d.length <= 253 && d !== "ts.net" &&
      /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/.test(d);
    // Match only at a label boundary.
    if (ok && (h === d || (h.length > d.length && h.slice(-(d.length + 1)) === "." + d))) {
      return true;
    }
  }
  return false;
}

// Exit mode proxies nonlocal destinations and leaves the user's LAN direct.
// Keep this self-contained, ASCII, and in parity with allowExitHost, as enforced
// by testdata/exit-mode-hosts.json.
function tailtabExitModeProxies(host, routes) {
  if (!host) return false;
  var h = String(host).toLowerCase();
  if (h.charAt(h.length - 1) === ".") h = h.slice(0, -1);
  if (h.charAt(0) === "[" && h.charAt(h.length - 1) === "]") h = h.slice(1, -1);
  if (h === "") return false;
  if (h.indexOf("%") !== -1) return false; // a zoned address, refused by hostAddr too

  var mapped = h.match(/^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/);
  if (mapped) h = mapped[1];

  if (h === "localhost") return false;
  if (h.length > 10 && h.slice(-10) === ".localhost") return false;

  var v4 = h.match(/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/);
  if (v4) {
    for (var oi = 1; oi <= 4; oi++) if (v4[oi].length > 1 && v4[oi].charAt(0) === "0") return false;
    var a = parseInt(v4[1], 10);
    var b = parseInt(v4[2], 10);
    if (a === 100 && b >= 64 && b <= 127) return true;
    // Local and reserved ranges outrank advertised routes, matching the host.
    if (a === 0 || a === 127) return false;
    if (a === 169 && b === 254) return false;
    if (a >= 224) return false;
    // Routed subnets go through their subnet router, not the exit node.
    if (tailtabInRoutes(h, routes)) return true;
    if (a === 10) return false;
    if (a === 172 && b >= 16 && b <= 31) return false;
    if (a === 192 && b === 168) return false;
    if (a === 169 && b === 254) return false;
    if (a >= 224) return false;
    return true;
  }

  if (h.indexOf(":") !== -1) {
    if (h.indexOf("fd7a:115c:a1e0") === 0) return true;
    // Reject every spelling of loopback before consulting routes.
    var hz = h.replace(/(^|:)0+(?=[0-9a-f])/g, "$1");
    var hs = hz.split(":").filter(function (g) { return g !== ""; });
    var allZero = true, lastOne = false;
    for (var gi = 0; gi < hs.length; gi++) {
      if (gi === hs.length - 1 && hs[gi] === "1") { lastOne = true; continue; }
      if (hs[gi] !== "0") { allZero = false; break; }
    }
    if (allZero && (hs.length === 0 || hs.length === 8 || h.indexOf("::") !== -1)) return false;
    if (h.indexOf("fe8") === 0 || h.indexOf("fe9") === 0) return false;
    if (h.indexOf("fea") === 0 || h.indexOf("feb") === 0) return false;
    if (h.indexOf("ff") === 0) return false;
    if (tailtabInRoutes(h, routes)) return true;
    if (h.indexOf("fc") === 0 || h.indexOf("fd") === 0) return false;
    return true;
  }

  // Reject numeric IPv4 obfuscations in parity with the host.
  if (/^\d+$/.test(h) || /^0x[0-9a-f]+$/.test(h)) return false;

  return true;
}

// Chromium cannot authenticate SOCKS5, so PAC uses HTTP PROXY and supplies its
// credential through onAuthRequired. CONNECT preserves unresolved MagicDNS names.
// Never add a DIRECT fallback: failed proxy authentication must fail closed.
function tailtabBuildPac(port, tailnetDomain, exitMode, routes, proxyHost) {
  var target = JSON.stringify("PROXY " + (proxyHost || "127.0.0.1") + ":" + port);
  // Admit only inert CIDR strings into generated PAC source.
  var clean = [];
  var list = Array.isArray(routes) ? routes : [];
  for (var i = 0; i < list.length; i++) {
    if (/^[0-9a-fA-F:.]+\/\d{1,3}$/.test(String(list[i]))) clean.push(String(list[i]).toLowerCase());
  }
  var routesJSON = JSON.stringify(clean);
  var pac;
  if (exitMode) {
    // Browser and host must derive exit mode from the same selected-node field.
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

  // Chromium rejects an entire PAC containing non-ASCII. Stringification embeds
  // function comments too, so enforce ASCII before installing routing.
  if (!/^[\x00-\x7f]*$/.test(pac)) {
    throw new Error("tailtab: the PAC script has a non-ASCII character in it, which Chromium rejects");
  }
  return pac;
}

if (typeof module !== "undefined" && module.exports) {
  module.exports = {
    tailtabInRoutes: tailtabInRoutes,
    tailtabIsTailnetHost: tailtabIsTailnetHost,
    tailtabExitModeProxies: tailtabExitModeProxies,
    tailtabBuildPac: tailtabBuildPac,
  };
}
