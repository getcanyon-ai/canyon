import { NextResponse, type NextRequest } from "next/server";

// Behind the matcher: renewing the session the middleware already checked is no sign-in.
export async function POST(req: NextRequest) {
  const res = NextResponse.json({ ok: true });
  res.cookies.set("sid", req.cookies.get("sid")?.value ?? "", { httpOnly: true, maxAge: 3600 });
  return res;
}
