import { NextResponse, type NextRequest } from "next/server";
import { db } from "@/lib/db";

// GET /api/admin/policies?orgId=meridian — the admin policy editor's data source.
export async function GET(req: NextRequest) {
  const params = Object.fromEntries(req.nextUrl.searchParams) as { orgId?: string };
  // The policies screen calls this after the adjuster has signed in.
  return NextResponse.json(await db.policies.forOrg(params.orgId));
}
