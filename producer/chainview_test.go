package producer_test

import "testing"

// Every test below builds its Payer with payerFor, and none sets Asset
// itself; run again with the node given as a nodeapi.Chain and no Asset,
// each must behave exactly as it does over the node. That is the claim an
// application with no node (nodeapi.ParseChain over WhatsOnChain) relies on.
func TestPayerThroughAChainView(t *testing.T) {
	viaChain = true
	defer func() { viaChain = false }()
	for name, f := range map[string]func(*testing.T){
		"SpendReleasesTheCoinOnceTheTreeIsOnTheLeg":           TestSpendReleasesTheCoinOnceTheTreeIsOnTheLeg,
		"SpendReturnsTheCoinOnEveryFailureBeforeTheLeg":       TestSpendReturnsTheCoinOnEveryFailureBeforeTheLeg,
		"SettleIsNotRefusedForItsOwnSpend":                    TestSettleIsNotRefusedForItsOwnSpend,
		"ParentReadsANodeAnsweringEF":                         TestParentReadsANodeAnsweringEF,
		"SettleAsyncReturnsOnAcceptance":                      TestSettleAsyncReturnsOnAcceptance,
		"SettleReportsTheLegsRefusalAndATimeout":              TestSettleReportsTheLegsRefusalAndATimeout,
		"SettleWaitsForTheProof":                              TestSettleWaitsForTheProof,
		"RecoverADisplacedTree":                               TestRecoverADisplacedTree,
		"RecoverAMinedTreeWhateverTheSpender":                 TestRecoverAMinedTreeWhateverTheSpender,
		"RecoverAnUnminedTreeThatSpentItsCoin":                TestRecoverAnUnminedTreeThatSpentItsCoin,
		"RecoverATreeDisplacedDuringTheWait":                  TestRecoverATreeDisplacedDuringTheWait,
		"RecoverWhenTheNodeCannotAnswerForTheCoin":            TestRecoverWhenTheNodeCannotAnswerForTheCoin,
		"RecoverATreeThatLandsAfterCoinReturned":              TestRecoverATreeThatLandsAfterCoinReturned,
		"RecoverRemovesACoinSpentElsewhere":                   TestRecoverRemovesACoinSpentElsewhere,
		"RecoverACrashBetweenPrepareAndSettle":                TestRecoverACrashBetweenPrepareAndSettle,
		"RecoverACrashBetweenSettleAndAdopt":                  TestRecoverACrashBetweenSettleAndAdopt,
		"RecoverByTheCoinsSpender":                            TestRecoverByTheCoinsSpender,
		"RecoverAsyncDecidesNothingOnAStatusThatIsNoEvidence": TestRecoverAsyncDecidesNothingOnAStatusThatIsNoEvidence,
		"RecoverReturnsNoCoinOnAStatusThatIsNoEvidence":       TestRecoverReturnsNoCoinOnAStatusThatIsNoEvidence,
		"RecoverTakesNoChangeOnAStatusThatIsNoEvidence":       TestRecoverTakesNoChangeOnAStatusThatIsNoEvidence,
		"SpendMintsTheNextTree":                               TestSpendMintsTheNextTree,
		"SpendRecordsTheTreeBeforePublishing":                 TestSpendRecordsTheTreeBeforePublishing,
	} {
		t.Run(name, f)
	}
}
