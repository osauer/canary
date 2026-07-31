let renderAllHandler = null;

// Feature modules can request a full repaint without importing app.js, the
// executable entrypoint. The entrypoint installs the production renderer once
// its module graph is ready; tests can install a scoped renderer while
// executing the same production modules.
function installRenderAll(handler) {
  if (typeof handler !== "function") {
    throw new TypeError("renderAll handler must be a function");
  }
  renderAllHandler = handler;
}

function renderAll() {
  if (!renderAllHandler) return;
  renderAllHandler();
}

export { installRenderAll, renderAll };
