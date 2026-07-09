// The dashboard's data shape and the single loader that assembles it. Kept out
// of the component so it can be unit-tested against a mock Api and reused by the
// refresh path.
import type { Api, BoxView, JoinTokenInfo, ProxyInfo, SpokeStatus } from "../api";

export interface DashboardData {
  spokes: SpokeStatus[];
  tokens: JoinTokenInfo[];
  boxes: BoxView[];
  /** Whether the hub has the reverse proxy configured at all; when false the
   * workspace details view hides its proxies section entirely. */
  proxyEnabled: boolean;
  proxies: ProxyInfo[];
}

/** loadDashboard fetches every section the dashboard shows in one pass. The
 * three independent lists load in parallel; the proxy list is only fetched when
 * the hub reports the proxy feature enabled, so a hub without it makes no call.
 *
 * @arg api The authenticated box-control API client.
 * @return Promise<DashboardData> The assembled dashboard state.
 */
export async function loadDashboard(api: Api): Promise<DashboardData> {
  const [spokes, tokens, boxes, proxyEnabled] = await Promise.all([
    api.spokeStatuses(),
    api.listJoinTokens(),
    api.listBoxes(),
    api.proxyEnabled(),
  ]);
  const proxies = proxyEnabled ? await api.listProxies() : [];
  return { spokes, tokens, boxes, proxyEnabled, proxies };
}

/** proxiesFor selects the proxies that belong to one workspace (box), by id.
 *
 * @arg proxies The full proxy list from the dashboard data.
 * @arg id The box id to filter on.
 * @return ProxyInfo[] The subset whose box_id matches.
 */
export function proxiesFor(proxies: ProxyInfo[], id: string): ProxyInfo[] {
  return proxies.filter((p) => p.box_id === id);
}
