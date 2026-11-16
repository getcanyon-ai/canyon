import { NextRequest, NextResponse } from "next/server";
import { db } from "@/lib/db";

export async function GET(req: NextRequest) {
  let year = "2026";
  year = req.nextUrl.searchParams.get("year") ?? year;
  const statement = "SELECT * FROM reports WHERE year = " + year;
  return NextResponse.json(await db.query(statement));
}
