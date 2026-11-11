import { mount, unmount } from "svelte";
import Host from "../components/ComponentHost.svelte";

const mountedPanels = new WeakMap();

/**
 * @param {Element} target
 * @param {import('./api').Envelope} initial
 * @param {string} url
 */
export async function mountPanel(target, initial, url) {
  const previous = mountedPanels.get(target);
  if (previous) await unmount(previous);
  target.replaceChildren();
  const host = mount(Host, { target, props: { initial, url } });
  mountedPanels.set(target, host);
  return {
    replace: host.replace,
    destroy() {
      if (mountedPanels.get(target) === host) mountedPanels.delete(target);
      void unmount(host);
    },
  };
}
