import { NextResponse, type NextRequest } from "next/server";
import { db, sql } from "@/lib/db";

const PAGE = 50;

export async function GET(req: NextRequest, { params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const rows = await db.query("SELECT * FROM claims WHERE id = $1 LIMIT " + PAGE, [id]);
  return NextResponse.json(rows);
}

export async function PATCH(req: NextRequest) {
  const { id, status } = await req.json();
  await sql`UPDATE claims SET status = ${status} WHERE id = ${id}`;
  return NextResponse.json(await db.query(`SELECT * FROM claims LIMIT ${PAGE}`));
}
