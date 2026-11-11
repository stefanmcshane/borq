import { mountPanel } from "./mount-panel.js";
import { request, type Envelope } from "./api";
import { trimFormInputs } from "./input";

type Replace = (value: Envelope) => void | Promise<void>;
const panels = new WeakMap<Element, Replace>();
export function registerPanel(node: HTMLElement, replace: Replace) {
  panels.set(node, replace);
  node.dataset.panelHost = "";
  return {
    update(next: Replace) {
      panels.set(node, next);
    },
    destroy() {
      panels.delete(node);
    },
  };
}
function targetFor(source: Element, selector?: string) {
  if (!selector || selector === "#workspace") return null;
  if (selector === "this") return source;
  if (selector.startsWith("closest ")) return source.closest(selector.slice(8));
  return document.querySelector(selector);
}
async function deliver(result: Envelope, source: Element, target?: string) {
  if (result.component === "workspace") {
    document.dispatchEvent(
      new CustomEvent("borq:workspace", { detail: result }),
    );
    return;
  }
  const editor =
    result.component === "application-editor" ||
    result.component === "environment-editor"
      ? "#" + result.component
      : target;
  const selected = targetFor(source, editor);
  if (selected && result.component === "custom-chart-defaults") {
    const mounted = await mountPanel(selected, result, "");
    panels.set(selected, mounted.replace);
    document.dispatchEvent(new CustomEvent("borq:render"));
    return;
  }
  let element = selected || source;
  while (element) {
    const replace = panels.get(element);
    if (replace) {
      await replace(result);
      return;
    }
    if (!element.parentElement) break;
    element = element.parentElement;
  }
  throw new Error(
    "The requested panel is no longer open. Refresh the workspace and try again.",
  );
}
type ErrorRecovery = { label: string; description: string } & (
  { href: string } | { focus: string }
);
function errorAlert(
  cause: unknown,
  retry: () => void,
  recovery?: ErrorRecovery,
) {
  const alert = document.createElement("div");
  alert.className = "alert request-error";
  alert.setAttribute("role", "alert");
  const message = document.createElement("p");
  const detail =
    cause instanceof Error
      ? cause.message
      : "Couldn’t reach Borq. Check your connection and try again.";
  message.textContent =
    detail === "request configuration is invalid" ||
    detail === "application configuration is invalid"
      ? "Some configuration settings are invalid."
      : detail;
  alert.append(message);
  if (recovery) {
    const help = document.createElement("p");
    help.className = "field-help";
    help.textContent = recovery.description;
    alert.append(help);
  }
  let action: HTMLAnchorElement | HTMLButtonElement;
  if (recovery && "href" in recovery) {
    const link = document.createElement("a");
    link.href = recovery.href;
    action = link;
  } else {
    const button = document.createElement("button");
    button.type = "button";
    button.onclick = () => {
      if (!recovery || !("focus" in recovery)) {
        retry();
        return;
      }
      const area = document.querySelector<HTMLElement>(recovery.focus);
      const field =
        area?.querySelector<HTMLElement>(
          "input:invalid, textarea:invalid, select:invalid",
        ) ||
        area?.querySelector<HTMLElement>(
          'input:not([type="hidden"]):not([readonly]):not(:disabled), textarea:not([readonly]):not(:disabled), select:not(:disabled)',
        ) ||
        area;
      field?.focus();
      field?.scrollIntoView({ block: "center" });
    };
    action = button;
  }
  action.className = "btn";
  action.dataset.variant = "outline";
  action.textContent = recovery?.label || "Try again";
  alert.append(action);
  return alert;
}
function showError(
  source: Element,
  cause: unknown,
  retry: () => void,
  recovery?: ErrorRecovery,
) {
  const form = source.closest("form");
  const target = form || document.getElementById("notice");
  if (!target) return;
  let errors = target.querySelector(".form-errors");
  if (!errors) {
    errors = document.createElement("div");
    errors.className = "form-errors";
    errors.setAttribute("aria-live", "polite");
    target.append(errors);
  }
  errors.replaceChildren(errorAlert(cause, retry, recovery));
}
function parameters(form: HTMLFormElement, submitter?: HTMLElement | null) {
  trimFormInputs(form);
  const data = new FormData(form);
  if (
    (submitter instanceof HTMLButtonElement ||
      submitter instanceof HTMLInputElement) &&
    submitter.name
  )
    data.append(submitter.name, submitter.value);
  const values = new URLSearchParams();
  for (const [key, value] of data)
    if (typeof value === "string") values.append(key, value);
  return values;
}
type FormOptions = {
  target?: string;
  confirm?: string;
  errorRecovery?: ErrorRecovery;
};
export function submitForm(form: HTMLFormElement, options: FormOptions = {}) {
  let pending = false;
  let controller: AbortController | undefined;
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (pending) return;
    if (options.confirm && !window.confirm(options.confirm)) return;
    const source = event.submitter;
    const address =
      source?.getAttribute("formaction") ||
      form.getAttribute("action") ||
      window.location.href;
    const method = (
      source?.getAttribute("formmethod") ||
      form.getAttribute("method") ||
      "POST"
    ).toUpperCase();
    const values = parameters(form, source);
    const url = new URL(address, window.location.href);
    if (method === "GET") url.search = values.toString();
    controller = new AbortController();
    pending = true;
    const buttons = Array.from(
      form.querySelectorAll<HTMLButtonElement>("button"),
    ).filter((button) => !button.disabled);
    form.querySelector(".form-errors")?.replaceChildren();
    for (const button of buttons) button.disabled = true;
    form.setAttribute("aria-busy", "true");
    try {
      const result = await request(url.pathname + url.search, {
        method,
        body: method === "GET" ? undefined : values,
        signal: controller.signal,
      });
      await deliver(result, source || form, options.target);
      if (
        result.component === "application-editor" ||
        result.component === "environment-editor"
      ) {
        const editor = document.getElementById(result.component);
        const focus = editor?.querySelector<HTMLElement>(
          '[data-focus], [name="editorFormat"][aria-current="page"], textarea, input:not([type="hidden"])',
        );
        focus?.focus({
          preventScroll: source?.getAttribute("name") === "editorFormat",
        });
      }
    } catch (cause) {
      if (!controller.signal.aborted)
        showError(
          source || form,
          cause,
          () => {
            form.requestSubmit(
              source instanceof HTMLButtonElement ||
                source instanceof HTMLInputElement
                ? source
                : undefined,
            );
          },
          options.errorRecovery,
        );
    } finally {
      pending = false;
      form.removeAttribute("aria-busy");
      for (const button of buttons) button.disabled = false;
    }
  }
  form.addEventListener("submit", submit);
  return {
    update(next: FormOptions) {
      options = next;
    },
    destroy() {
      controller?.abort();
      form.removeEventListener("submit", submit);
    },
  };
}
type RequestOptions = {
  url: string;
  method: string;
  target?: string;
  errorRecovery?: ErrorRecovery;
};
export function requestButton(
  button: HTMLButtonElement,
  options: RequestOptions,
) {
  let controller: AbortController | undefined;
  async function click(event: MouseEvent) {
    event.preventDefault();
    if (button.disabled) return;
    controller?.abort();
    controller = new AbortController();
    button.disabled = true;
    const defaults = button.hasAttribute("data-custom-chart-defaults");
    const form = button.form;
    const reference = () =>
      form
        ? JSON.stringify(
            ["repository", "chart", "version"].map((name) =>
              parameters(form).get(name),
            ),
          )
        : "";
    const requestedReference = defaults ? reference() : "";
    if (defaults)
      document.getElementById("custom-chart-defaults-popout")?.showPopover();
    try {
      const body =
        options.method === "POST" && button.form
          ? parameters(button.form)
          : undefined;
      const result = await request(options.url, {
        method: options.method,
        body,
        signal: controller.signal,
      });
      if (!defaults || requestedReference === reference()) {
        await deliver(result, button, options.target);
        document
          .getElementById("custom-chart-defaults-popout")
          ?.querySelector<HTMLElement>("[data-focus]")
          ?.focus({ preventScroll: true });
      }
    } catch (cause) {
      if (!controller.signal.aborted)
        showError(button, cause, () => button.click(), options.errorRecovery);
    } finally {
      button.disabled = false;
    }
  }
  button.addEventListener("click", click);
  return {
    update(next: RequestOptions) {
      options = next;
    },
    destroy() {
      controller?.abort();
      button.removeEventListener("click", click);
    },
  };
}
type LoadOptions = { url: string; lazy?: boolean; interval?: number };
export function loadPanel(node: HTMLElement, options: LoadOptions) {
  let controller = new AbortController();
  let mounted: Awaited<ReturnType<typeof mountPanel>> | undefined;
  let started = false;
  let generation = 0;
  let stopped = false;

  async function replace(result: Envelope) {
    if (stopped) return;
    controller.abort();
    const version = ++generation;
    started = true;
    node.removeAttribute("aria-busy");
    node.removeAttribute("role");
    if (mounted) {
      await mounted.replace(result);
    } else {
      const next = await mountPanel(node, result, options.url);
      if (stopped || version !== generation) {
        next.destroy();
        return;
      }
      mounted = next;
    }
    document.dispatchEvent(new CustomEvent("borq:render"));
  }
  panels.set(node, replace);

  async function load() {
    if (started || stopped) return;
    started = true;
    const version = ++generation;
    const signal = controller.signal;
    node.setAttribute("aria-busy", "true");
    try {
      const result = await request(options.url, { signal });
      if (!signal.aborted && version === generation) await replace(result);
    } catch (cause) {
      if (!signal.aborted && version === generation) {
        started = false;
        node.replaceChildren(errorAlert(cause, () => void load()));
      }
    } finally {
      if (version === generation) node.removeAttribute("aria-busy");
    }
  }
  let observer: IntersectionObserver | undefined;
  if (options.lazy) {
    observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting)) {
        observer?.disconnect();
        void load();
      }
    });
    observer.observe(node);
  } else void load();
  return {
    update(next: LoadOptions) {
      if (next.url === options.url) {
        options = next;
        return;
      }
      controller.abort();
      controller = new AbortController();
      generation++;
      mounted?.destroy();
      mounted = undefined;
      options = next;
      started = false;
      observer?.disconnect();
      void load();
    },
    destroy() {
      stopped = true;
      generation++;
      observer?.disconnect();
      controller.abort();
      mounted?.destroy();
      panels.delete(node);
    },
  };
}
