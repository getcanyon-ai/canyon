export async function GET(req: Request, { params }: { params: { id: string } }) {
  return Response.json(await db.$queryRawUnsafe(`DELETE FROM t WHERE id = ${params.id}`));
}
