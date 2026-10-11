"use client";

import { useEffect, useState } from "react";
import { desktop } from "@/lib/desktop";
import {
  setSystemNotifications, systemNotificationsBlocked, systemNotificationsEnabled, systemNotificationsSupported,
} from "@/lib/notifications";
import { Checkbox } from "@/components/ui";

/**
 * Lets the user choose whether new notifications are also shown outside
 * the page while the window is in the background. The choice belongs to
 * this browser, or to the desktop application, not to the account.
 */
export default function SystemNotifications() {
  // What the environment offers is only known once the page is in it.
  const [state, setState] = useState<{ supported: boolean; enabled: boolean; blocked: boolean; inApp: boolean } | null>(null);
  const read = () => setState({
    supported: systemNotificationsSupported(), enabled: systemNotificationsEnabled(),
    blocked: systemNotificationsBlocked(), inApp: !!desktop(),
  });
  useEffect(read, []);

  const change = async (on: boolean) => {
    await setSystemNotifications(on);
    read();
  };

  if (!state?.supported) return null;
  return (
    <div className="card">
      <div className="card-head"><h2>Notifications</h2></div>
      <div className="card-body">
        <Checkbox
          label={state.inApp ? "Desktop notifications" : "Desktop notifications in this browser"}
          hint={state.blocked
            ? "This browser has been told not to show notifications from Dedie. Allow them in the browser's site settings, then switch this on."
            : "When a migration finishes or fails, a backup fails or a database stops answering while this window is in the background, your computer shows a notification. The bell at the top of the page shows them either way."}
          checked={state.enabled}
          disabled={state.blocked}
          onChange={(on) => void change(on)}
        />
      </div>
    </div>
  );
}
