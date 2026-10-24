(function () {
  const root = document.documentElement;
  const storageKey = "borq.theme";

  function applyTheme(theme) {
    const changed = root.dataset.theme !== theme;
    if (changed) root.setAttribute("data-theme-changing", "");
    root.classList.toggle("dark", theme === "dark");
    root.dataset.theme = theme;
    const label = theme === "dark" ? "Switch to light theme" : "Switch to dark theme";
    document.querySelectorAll("[data-theme-toggle]").forEach(function (button) {
      button.setAttribute("aria-label", label);
      button.setAttribute("title", label);
    });
    if (changed) requestAnimationFrame(function () {
      requestAnimationFrame(function () { root.removeAttribute("data-theme-changing"); });
    });
  }

  let theme = "dark";
  try {
    if (localStorage.getItem(storageKey) === "light") theme = "light";
  } catch (_) {
    // The theme still works when browser storage is unavailable.
  }
  applyTheme(theme);

  document.addEventListener("DOMContentLoaded", function () {
    applyTheme(root.dataset.theme);
  });

  document.addEventListener("htmx:afterSwap", function () {
    applyTheme(root.dataset.theme);
  });

  document.addEventListener("click", function (event) {
    if (!event.target.closest("[data-theme-toggle]")) return;
    const next = root.dataset.theme === "dark" ? "light" : "dark";
    applyTheme(next);
    try {
      localStorage.setItem(storageKey, next);
    } catch (_) {
      // Storage restrictions should not prevent switching the current page.
    }
  });

  window.addEventListener("storage", function (event) {
    if (event.key === storageKey) applyTheme(event.newValue === "light" ? "light" : "dark");
  });
})();
