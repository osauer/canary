// Language preference for the generated documentation pages.
//
// The site is static, so nothing on the server reads Accept-Language. On a
// reader's first visit the browser's language list decides: a browser that
// prefers Chinese is sent once to the page in its own script (Traditional or
// Simplified), when there is one, and that becomes the stored choice. From then on the
// stored choice rules, and the selector in the header (and every link that
// carries data-lang) changes it. A page that exists in one language only
// never redirects, so a search result always opens.
(function () {
  var key = "canary-lang";
  var html = document.documentElement;
  var here = html.getAttribute("lang") || "en";
  var available = {};
  var links = document.querySelectorAll('link[rel="alternate"][hreflang]');
  for (var i = 0; i < links.length; i++) {
    var tag = links[i].getAttribute("hreflang");
    if (tag && tag !== "x-default") available[tag] = links[i].getAttribute("href");
  }
  var stored = null;
  try { stored = localStorage.getItem(key); } catch (e) { /* storage blocked: decide per page */ }
  if (!stored) {
    // First visit: a browser that prefers Chinese is sent to the page in its
    // own script, when there is one, and that becomes the stored choice.
    // zh-HK, zh-TW and zh-MO read Traditional; zh-CN, zh-SG and bare zh read
    // Simplified. Any other first visit shows the page that was asked for,
    // so a shared Chinese link opens in Chinese everywhere.
    var prefers = navigator.languages || [navigator.language || ""];
    var chosen = "";
    for (var j = 0; j < prefers.length; j++) {
      var tag = String(prefers[j]).toLowerCase();
      if (/^en(-|$)/.test(tag)) break;
      if (/^zh(-|$)/.test(tag)) {
        chosen = /hant|-hk|-tw|-mo/.test(tag) ? "zh-Hant" : "zh-Hans";
        break;
      }
    }
    if (chosen && chosen !== here && available[chosen]) {
      try { localStorage.setItem(key, chosen); } catch (e) { /* same */ }
      location.replace(available[chosen]);
      return;
    }
  } else if (stored !== here && available[stored]) {
    // A stored choice rules: the selector and every data-lang link set it.
    location.replace(available[stored]);
    return;
  }
  document.addEventListener("click", function (event) {
    var target = event.target;
    while (target && target !== document && !target.hasAttribute("data-lang")) target = target.parentNode;
    if (!target || target === document) return;
    try { localStorage.setItem(key, target.getAttribute("data-lang")); } catch (e) { /* same */ }
  });
})();
