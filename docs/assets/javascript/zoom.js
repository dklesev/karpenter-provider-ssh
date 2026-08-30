/* Click-to-enlarge for mermaid diagrams.
 *
 * Zensical renders each diagram into a CLOSED shadow root on a div.mermaid
 * host — the inner SVG is unreachable from page scripts, so lightbox-style
 * cloning is impossible. Instead the host element itself is scaled with a
 * CSS transform (shadow content scales along) and centered in the viewport
 * over a backdrop. Event delegation on document survives instant-navigation
 * page swaps without re-binding.
 */
(function () {
  "use strict";

  var zoomed = null; // { host, backdrop }

  function close() {
    if (!zoomed) return;
    zoomed.host.classList.remove("dz-zoomed");
    zoomed.host.style.transform = "";
    zoomed.backdrop.remove();
    document.body.style.overflow = "";
    zoomed = null;
  }

  function open(host) {
    close();

    var rect = host.getBoundingClientRect();
    if (!rect.width || !rect.height) return;
    var k = Math.min(
      (window.innerWidth * 0.94) / rect.width,
      (window.innerHeight * 0.92) / rect.height,
      8
    );
    if (k <= 1.02) k = Math.min(2, k * 2); // tiny diagrams still get a step up

    var tx = window.innerWidth / 2 - (rect.left + rect.width / 2);
    var ty = window.innerHeight / 2 - (rect.top + rect.height / 2);

    var backdrop = document.createElement("div");
    backdrop.className = "dz-backdrop";
    backdrop.addEventListener("click", close);
    document.body.appendChild(backdrop);

    host.classList.add("dz-zoomed");
    host.style.transform =
      "translate(" + tx + "px," + ty + "px) scale(" + k + ")";
    document.body.style.overflow = "hidden";

    zoomed = { host: host, backdrop: backdrop };
  }

  document.addEventListener("click", function (e) {
    var host = e.target.closest(".md-content div.mermaid");
    if (!host) return;
    if (zoomed && zoomed.host === host) {
      close();
      return;
    }
    open(host);
  });

  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape") close();
  });

  // Any scroll/resize invalidates the computed transform — just close.
  window.addEventListener("resize", close);
})();
