import { NextResponse } from "next/server";

// Behind the middleware: the matcher sends /api/admin/* through the session check.
export async function GET() {
  return NextResponse.json({ entries: [] });
}
