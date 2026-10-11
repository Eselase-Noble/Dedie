"use client";

import { api } from "@/lib/api";
import { useLoad } from "@/lib/hooks";
import Password from "@/components/account/Password";
import Sessions from "@/components/account/Sessions";
import Tokens from "@/components/account/Tokens";
import TwoFactor from "@/components/account/TwoFactor";
import { useSession } from "@/components/session";
import { Banner, PageHeader } from "@/components/ui";

/** The signed-in user's own security settings. */
export default function AccountPage() {
  const { session } = useSession();
  // Two-factor state changes on this page, so it is read fresh rather
  // than taken from the session loaded when the shell started.
  const { data, error, reload } = useLoad(async () => {
    const [me, policy] = await Promise.all([api.me(), api.policy()]);
    return { me, policy };
  }, []);
  const enabled = data?.me.user.totp_enabled ?? session.user.totp_enabled;

  // Leaving enrolment reloads the shell so the rest of the application
  // becomes available.
  const onTwoFactorChange = async () => {
    if (session.must_enroll_totp) {
      window.location.href = "/";
      return;
    }
    await reload();
  };

  return (
    <div className="content">
      <PageHeader title="Account" subtitle={`${session.user.name} · ${session.user.email} · ${session.user.role}`} />
      {error && <Banner tone="error">{error}</Banner>}
      <TwoFactor enabled={enabled} required={data?.policy.require_totp ?? false} onChange={() => void onTwoFactorChange()} />
      <Password policy={data?.policy ?? null} />
      {!session.must_enroll_totp && <Sessions />}
      {!session.must_enroll_totp && <Tokens />}
    </div>
  );
}
