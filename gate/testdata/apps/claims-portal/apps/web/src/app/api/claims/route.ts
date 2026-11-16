import { NextResponse, type NextRequest } from "next/server";
import { db } from "@/lib/db";

export async function GET(req: NextRequest) {
  if (!req.cookies.get("cp_session")) {
    return NextResponse.json({ error: "sign in first" }, { status: 401 });
  }
  return NextResponse.json(await db.claims.search(req.nextUrl.searchParams.get("q") ?? ""));
}
