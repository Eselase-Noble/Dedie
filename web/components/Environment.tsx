"use client";

import { useCallback, useRef, useState, type ReactNode } from "react";
import { asksForProductionName, errorMessage } from "@/lib/api";
import type { Connection, Environment } from "@/lib/types";
import { ConfirmDialog } from "./ui";

export const ENVIRONMENTS: { value: Environment; label: string; hint: string }[] = [
  { value: "", label: "Not set", hint: "Treated like any connection that is not production." },
  { value: "production", label: "Production", hint: "Destructive and disruptive actions ask for this connection's name first." },
  { value: "staging", label: "Staging", hint: "Marked so it is not mistaken for production." },
  { value: "development", label: "Development", hint: "Marked so it is not mistaken for production." },
];

/** The label of an environment; nothing for a connection that has none. */
export function EnvBadge({ env }: { env?: Environment | string }) {
  if (!env) return null;
  return <span className={`env ${env}`}>{env}</span>;
}

/** A connection's name with its environment beside it, wherever a connection is named. */
export function ConnectionName({ connection, name, strong }: { connection?: Pick<Connection, "name" | "environment"> | null; name?: string; strong?: boolean }) {
  const text = connection?.name ?? name ?? "";
  return (
    <span className="conn-name">
      {strong ? <strong>{text}</strong> : <span>{text}</span>}
      <EnvBadge env={connection?.environment} />
    </span>
  );
}

/** A connection as one line of text, for the options of a select. */
export function connectionOption(c: Connection, detail = `${c.kind}, ${c.database}`): string {
  const env = c.environment === "production" ? " · PRODUCTION" : c.environment ? ` · ${c.environment}` : "";
  return `${c.name}${env} (${detail})`;
}

/**
 * Asks for a production connection's name before something that destroys
 * or disrupts. The button stays off until the name is typed exactly; the
 * action then runs with it, and a failure is shown here for another try.
 */
export function ProductionConfirm({ name, title, children, confirmLabel, onConfirm, onClose }: {
  name: string; title?: string; children?: ReactNode; confirmLabel?: string;
  onConfirm: (typed: string) => Promise<void>; onClose: () => void;
}) {
  return (
    <ConfirmDialog title={title ?? "This is a production connection"} confirmLabel={confirmLabel ?? "Confirm"} typeName={name}
      onClose={onClose} onConfirm={() => onConfirm(name)}>
      {children}
    </ConfirmDialog>
  );
}

interface Pending {
  name: string;
  message: string;
  retry: (typed: string) => Promise<void>;
  cancel: () => void;
}

/**
 * Runs actions that a production connection lets through only once its
 * name has been typed. The action is tried as it is; the server knows
 * which connections and which actions ask for the name, and when it does,
 * the dialog opens and the action is tried again with what was typed.
 *
 * `guarded` resolves to the action's result, or to undefined when the user
 * closed the dialog instead. Render `dialog` somewhere in the component.
 */
export function useProductionGuard() {
  const [pending, setPending] = useState<Pending | null>(null);
  const label = useRef("Confirm");

  const guarded = useCallback(async <T,>(name: string, action: (confirm?: string) => Promise<T>, confirmLabel = "Confirm"): Promise<T | undefined> => {
    try {
      return await action();
    } catch (err) {
      if (!asksForProductionName(err)) throw err;
      label.current = confirmLabel;
      return new Promise<T | undefined>((resolve, reject) => {
        setPending({
          name,
          message: errorMessage(err),
          // The dialog closes itself once the retry is over, and a
          // promise that is already settled ignores the second answer.
          // A failure that has nothing to do with the name, such as an
          // error in the statement, goes back to whoever asked.
          retry: async (typed) => {
            try {
              resolve(await action(typed));
            } catch (failure) {
              if (asksForProductionName(failure)) throw failure;
              reject(failure);
            }
          },
          cancel: () => {
            setPending(null);
            resolve(undefined);
          },
        });
      });
    }
  }, []);

  const dialog = pending && (
    <ProductionConfirm name={pending.name} confirmLabel={label.current} onConfirm={pending.retry} onClose={pending.cancel}>
      <p>{pending.message}</p>
    </ProductionConfirm>
  );
  return { guarded, dialog };
}
