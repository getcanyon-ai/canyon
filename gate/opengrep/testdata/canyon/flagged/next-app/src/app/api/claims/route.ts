import { NextResponse, type NextRequest } from "next/server";
import { db } from "@/lib/db";

export async function GET(req: NextRequest) {
  const q = req.nextUrl.searchParams.get("q") ?? "";
  const rows = await db.query(`SELECT * FROM claims WHERE claimant ILIKE '%${q}%'`);
  return NextResponse.json(rows);
}
