"use client";

import { api } from "@/lib/api";
import { formatTime } from "@/lib/format";
import { useLoad } from "@/lib/hooks";
import { Badge, Banner, Loading } from "@/components/ui";

/** Reduces a user-agent string to "Browser on System". */
function describeAgent(agent: string): string {
  if (!agent) return "Unknown device";
  if (agent.includes("JavaFX")) return "Dedie desktop";
  const browser = /Edg\//.test(agent) ? "Edge" : /Firefox\//.test(agent) ? "Firefox" : /Chrome\//.test(agent) ? "Chrome"
    : /Safari\//.test(agent) ? "Safari" : /^Go-http|^dedie/.test(agent) ? "Dedie CLI" : "Browser";
  const system = /Windows/.test(agent) ? "Windows" : /Mac OS X/.test(agent) ? "macOS" : /Android/.test(agent) ? "Android"
    : /iPhone|iPad/.test(agent) ? "iOS" : /Linux/.test(agent) ? "Linux" : "";
  return system ? `${browser} on ${system}` : browser;
}

export default function Sessions() {
  const { data, error, loading, reload } = useLoad(() => api.sessions(), []);
  const revoke = async (id: string) => {
    await api.revokeSession(id);
    await reload();
  };
  return (
    <div className="card">
      <div className="card-head"><h2>Signed-in sessions</h2></div>
      <div className="card-body flush table-wrap">
        {error && <Banner tone="error">{error}</Banner>}
        {loading ? <Loading /> : (
          <table className="data stack">
            <thead><tr><th>Device</th><th>Address</th><th>Signed in</th><th>Last active</th><th>Expires</th><th /></tr></thead>
            <tbody>
              {data?.map((s) => (
                <tr key={s.id || s.created_at}>
                  <td title={s.user_agent}>{describeAgent(s.user_agent)} {s.current && <Badge tone="ok">this session</Badge>}</td>
                  <td className="mono">{s.ip}</td>
                  <td className="nowrap muted">{formatTime(s.created_at)}</td>
                  <td className="nowrap muted">{formatTime(s.last_seen_at)}</td>
                  <td className="nowrap muted">{formatTime(s.expires_at)}</td>
                  <td>
                    <div className="row-actions">
                      {!s.current && s.id && <button className="btn small" onClick={() => revoke(s.id)}>Sign out</button>}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
