"use client";

import { createContext, useContext } from "react";
import type { Permission, Session } from "@/lib/types";

interface SessionValue {
  session: Session;
  can: (permission: Permission) => boolean;
}

export const SessionContext = createContext<SessionValue | null>(null);

/** The signed-in user and a permission check. Available inside the app shell. */
export function useSession(): SessionValue {
  const value = useContext(SessionContext);
  if (!value) throw new Error("useSession must be used inside the app shell");
  return value;
}
