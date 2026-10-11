import type { ReactNode } from "react";
import Shell from "@/components/Shell";

export default function AppLayout({ children }: { children: ReactNode }) {
  return <Shell>{children}</Shell>;
}
