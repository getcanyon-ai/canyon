import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = { title: "Meridian Claims Portal" };

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <header>
          <strong>Meridian</strong> Claims Portal
        </header>
        <main>{children}</main>
      </body>
    </html>
  );
}
