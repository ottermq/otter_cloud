# Aviso de privacidade da Otter Labs Cloud

> **Rascunho para revisão (#125).** Os trechos entre `<…>` são preenchidos
> pelo responsável antes da publicação.

Última atualização: `<data da publicação>`.

Este aviso explica quais dados pessoais a Otter Labs Cloud trata sobre
quem tem uma conta, por quê, por quanto tempo e como exercer seus direitos
pela Lei Geral de Proteção de Dados (Lei nº 13.709/2018, LGPD). O conteúdo
dos bancos que você cria é tratado pelos [Termos de uso](termos-beta.md),
seção 7.

## 1. Controlador

`<nome completo>`, pessoa física, responsável pelo projeto Otter Labs.
Contato, inclusive para pedidos sobre seus dados: `<e-mail de contato>`.

## 2. Quais dados tratamos

| Dado | De onde vem |
|---|---|
| Endereço de e-mail | O convite e o cadastro |
| Senha, guardada apenas como hash (argon2id); a senha em si nunca é armazenada | O cadastro e as trocas de senha |
| Sessões: data de início e de último uso, endereço IP de onde a sessão foi aberta, identificação do navegador (user agent) | Cada login |
| Registro de ações (auditoria): o que foi feito (login, criação ou exclusão de banco, troca de senha…), quando, e o endereço IP | Cada ação no portal |
| Dados técnicos dos seus bancos: nome, tamanho, estado, datas | O uso do serviço |

**Não coletamos** nome, CPF, telefone, endereço ou dados de pagamento. O
portal usa **um único cookie**, o da sessão, necessário para manter você
conectado; não há cookies de publicidade nem ferramentas de análise de
terceiros.

## 3. Para que e com que base legal

| Finalidade | Dados | Base legal (LGPD, art. 7º) |
|---|---|---|
| Criar e manter sua conta, autenticar você, prestar o serviço | E-mail, senha, sessões, dados dos bancos | Execução de contrato (V) |
| Segurança: limitar tentativas de login, detectar e investigar abusos e incidentes, deixar você ver e encerrar suas sessões | Endereços IP, user agent, registro de ações | Legítimo interesse (IX) |
| Falar com você sobre o serviço (avisos, mudanças, incidentes) | E-mail | Execução de contrato (V) |
| Cumprir obrigação legal ou ordem judicial | O necessário em cada caso | Obrigação legal (II) |

Não vendemos dados, não os usamos para publicidade e não tomamos decisões
automatizadas sobre você.

## 4. Onde ficam

Os servidores ficam no **Brasil**: o servidor onde rodam os bancos e a
plataforma, e as cópias de segurança. O ponto de entrada público, que
recebe as conexões ao portal e aos bancos e as repassa por um túnel
cifrado, fica em `<país do servidor de entrada>`.

O servidor de entrada decifra cada conexão e a cifra de novo até os
servidores; ele é um servidor virtual alugado, cujo provedor tem acesso
físico à máquina, mas não à plataforma nem às contas. Fora isso, não
compartilhamos seus dados com terceiros, exceto com autoridades quando a lei
exigir.

## 5. Por quanto tempo

| Dado | Mantido |
|---|---|
| E-mail, hash da senha, dados dos bancos | Enquanto a conta existir |
| Sessões (com IP e user agent) | Até 90 dias depois de encerradas ou expiradas |
| Endereço IP no registro de ações | 12 meses; depois o registro fica sem o IP |
| Tudo, depois que você pedir a exclusão da conta | Apagado em até 30 dias, exceto o que a lei nos obrigue a guardar |
| Cópias de segurança do banco da plataforma, que contêm os dados acima | Até 14 dias, cifradas |
| Snapshots do servidor (seção 4 dos Termos) | Até 13 meses |

Uma cópia de segurança não é alterada depois de feita: um dado apagado
continua nas cópias até elas expirarem, nos prazos acima, e não é usado
para outro fim nesse tempo.

## 6. Segurança

Senhas guardadas só como hash; conexões cifradas (TLS) do seu navegador ou
aplicação até o servidor de entrada, e de novo dele até os servidores, por
um túnel cifrado; cópias de segurança cifradas antes de sair do servidor;
isolamento de rede entre os bancos de clientes, verificado a cada 30
minutos; acesso administrativo restrito ao responsável. Se houver um
incidente de segurança que possa causar risco ou dano relevante,
avisaremos os titulares afetados e a Autoridade Nacional de Proteção de
Dados (ANPD), como exige a LGPD.

## 7. Seus direitos

Pela LGPD (art. 18), você pode pedir: confirmação de que tratamos seus
dados e acesso a eles; correção; anonimização, bloqueio ou eliminação de
dados desnecessários ou excessivos; portabilidade; eliminação dos dados
tratados com seu consentimento; informação sobre com quem os
compartilhamos; e revisão desta forma de tratamento. Você também pode se
opor a um tratamento baseado em legítimo interesse.

Escreva para `<e-mail de contato>` a partir do e-mail da conta.
Responderemos em até 15 dias. Você também pode reclamar à ANPD
(<https://www.gov.br/anpd>).

## 8. Alterações

Se este aviso mudar de forma relevante, avisaremos por e-mail antes de a
mudança valer. A data no topo indica a versão.
