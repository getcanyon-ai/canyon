import { NextRequest, NextResponse } from "next/server";
import { db } from "@/lib/db";
import { searchStatement } from "@/lib/query";

export async function GET(req: NextRequest) {
  const term = req.nextUrl.searchParams.get("q") ?? "";
  return NextResponse.json(await db.query(searchStatement(term)));
}
