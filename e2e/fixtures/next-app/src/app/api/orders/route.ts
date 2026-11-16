import { NextResponse, type NextRequest } from "next/server";

// Outside the matcher, so it checks the session itself before anything else.
export async function GET(req: NextRequest) {
  if (!req.cookies.get("sid")) {
    return NextResponse.json({ error: "sign in first" }, { status: 401 });
  }
  return NextResponse.json({ orders: [] });
}
