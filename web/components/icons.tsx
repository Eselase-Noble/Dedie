import type { ReactNode } from "react";

// The interface's own icons: sixteen-pixel line drawings that take the
// colour of the text around them. They are decoration; whatever carries one
// has a label of its own, so they are hidden from assistive technology.
function Icon({ children, size = 16 }: { children: ReactNode; size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5"
      strokeLinecap="round" strokeLinejoin="round" aria-hidden="true" focusable="false">
      {children}
    </svg>
  );
}

type Props = { size?: number };

/** The Dedie mark: a shield, for "safe", holding a database. */
export function Logo({ size = 28 }: Props) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" focusable="false">
      <path fill="currentColor" d="M16 2.5 4.5 6.6v8.2c0 7 4.7 12.3 11.5 14.7 6.8-2.4 11.5-7.7 11.5-14.7V6.6L16 2.5Z" />
      <g fill="none" stroke="#fff" strokeWidth="1.7" strokeLinecap="round">
        <ellipse cx="16" cy="11" rx="5.6" ry="2.3" />
        <path d="M10.4 11v4.6c0 1.3 2.5 2.3 5.6 2.3s5.6-1 5.6-2.3V11" />
        <path d="M10.4 15.6v4.3c0 1.3 2.5 2.3 5.6 2.3s5.6-1 5.6-2.3v-4.3" />
      </g>
    </svg>
  );
}

export const HomeIcon = (p: Props) => <Icon {...p}><path d="M2.5 7.2 8 2.5l5.5 4.7v5.6a.7.7 0 0 1-.7.7H9.9V9.7H6.1v3.8H3.2a.7.7 0 0 1-.7-.7V7.2Z" /></Icon>;
export const PlugIcon = (p: Props) => <Icon {...p}><path d="M5.5 2v3M10.5 2v3M4 5h8v2.5a4 4 0 0 1-8 0V5ZM8 11.5V14" /></Icon>;
export const TerminalIcon = (p: Props) => <Icon {...p}><rect x="2" y="2.5" width="12" height="11" rx="1.6" /><path d="m5 6.2 2 1.8-2 1.8M8.6 10h2.4" /></Icon>;
export const TransferIcon = (p: Props) => <Icon {...p}><path d="M2.5 5.5h10M10 3l2.5 2.5L10 8M13.5 10.5h-10M6 8l-2.5 2.5L6 13" /></Icon>;
export const ArchiveIcon = (p: Props) => <Icon {...p}><rect x="2" y="2.5" width="12" height="3.2" rx="0.8" /><path d="M3 5.7v6.6a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1V5.7M6.5 8.5h3" /></Icon>;
export const PulseIcon = (p: Props) => <Icon {...p}><path d="M1.5 8.5h3l1.6-4.5 3 8.5L11 8.5h3.5" /></Icon>;
export const ServerIcon = (p: Props) => <Icon {...p}><rect x="2" y="2.5" width="12" height="4.5" rx="1" /><rect x="2" y="9" width="12" height="4.5" rx="1" /><path d="M4.6 4.75h.01M4.6 11.25h.01" /></Icon>;
export const KeyIcon = (p: Props) => <Icon {...p}><circle cx="5.2" cy="10.8" r="2.7" /><path d="m7.2 8.8 5.8-5.8M10.6 5.4l1.8 1.8M12.2 3.8 13.5 5" /></Icon>;
export const UsersIcon = (p: Props) => <Icon {...p}><circle cx="6" cy="5.2" r="2.4" /><path d="M1.8 13.2c.3-2.3 2-3.7 4.2-3.7s3.9 1.4 4.2 3.7M10.6 3.1a2.3 2.3 0 0 1 0 4.3M12 9.8c1.3.5 2.1 1.7 2.3 3.4" /></Icon>;
export const ListIcon = (p: Props) => <Icon {...p}><path d="M5.5 4h8M5.5 8h8M5.5 12h8M2.5 4h.01M2.5 8h.01M2.5 12h.01" /></Icon>;
export const UserIcon = (p: Props) => <Icon {...p}><circle cx="8" cy="5.3" r="2.6" /><path d="M3 13.5c.400-2.6 2.4-4.1 5-4.1s4.6 1.5 5 4.1" /></Icon>;
export const SignOutIcon = (p: Props) => <Icon {...p}><path d="M6.5 2.5H3.7a1 1 0 0 0-1 1v9a1 1 0 0 0 1 1h2.8M10 5l3 3-3 3M13 8H6.5" /></Icon>;
export const CloseIcon = (p: Props) => <Icon {...p}><path d="m4 4 8 8M12 4l-8 8" /></Icon>;
export const MenuIcon = (p: Props) => <Icon {...p}><path d="M2.5 4.5h11M2.5 8h11M2.5 11.5h11" /></Icon>;
export const SidebarIcon = (p: Props) => <Icon {...p}><rect x="2" y="2.5" width="12" height="11" rx="1.6" /><path d="M6 2.5v11" /></Icon>;
export const KeyboardIcon = (p: Props) => <Icon {...p}><rect x="1.5" y="4" width="13" height="8.5" rx="1.4" /><path d="M4.2 6.8h.01M6.7 6.8h.01M9.3 6.8h.01M11.8 6.8h.01M5 9.8h6" /></Icon>;
export const AlertIcon = (p: Props) => <Icon {...p}><path d="M8 2.3 14.2 13H1.8L8 2.3ZM8 6.5v3M8 11.3h.01" /></Icon>;
export const CheckIcon = (p: Props) => <Icon {...p}><path d="m3.5 8.3 3 3 6-6.6" /></Icon>;
export const ArrowLeftIcon = (p: Props) => <Icon {...p}><path d="M13 8H3.5M7 4.3 3.3 8 7 11.7" /></Icon>;
export const PlusIcon = (p: Props) => <Icon {...p}><path d="M8 3.3v9.4M3.3 8h9.4" /></Icon>;
export const BellOffIcon = (p: Props) => <Icon {...p}><path d="M4 11.5V7a4 4 0 0 1 8 0v4.5l1 1.5H3l1-1.5ZM6.7 14.5h2.6" /></Icon>;
