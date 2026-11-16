import { NextResponse, type NextRequest } from "next/server";

// Demo sign-in: any email and password start a session.
export async function POST(req: NextRequest) {
  const form = await req.formData();
  const next = String(form.get("next") ?? "/");
  const res = NextResponse.redirect(new URL(next.startsWith("/") ? next : "/", req.url), 303);
  res.cookies.set("cp_session", String(form.get("email") ?? "adjuster"), { httpOnly: true, sameSite: "lax", path: "/" });
  return res;
}
