import { NextResponse, type NextRequest } from "next/server";
import { Prisma } from "@prisma/client";
import { prisma } from "@/lib/prisma";

export async function GET(req: NextRequest) {
  const customer = req.nextUrl.searchParams.get("customer");
  const where = Prisma.sql`WHERE customer = ${customer}`;
  return NextResponse.json(await prisma.$queryRaw`SELECT * FROM orders ${where}`);
}
